// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/dbdesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemadesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/memsize"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/roleoption"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sessioninit"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil/singleflight"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
)

// MembershipCache is a shared cache for role membership information.
type MembershipCache struct {
	syncutil.Mutex
	tableVersion descpb.DescriptorVersion
	boundAccount mon.BoundAccount
	// userCache is a mapping from username to userRoleMembership.
	userCache map[security.SQLUsername]userRoleMembership
	// populateCacheGroup ensures that there is at most one request in-flight
	// for each key.
	populateCacheGroup singleflight.Group
	stopper            *stop.Stopper
}

// NewMembershipCache initializes a new MembershipCache.
func NewMembershipCache(account mon.BoundAccount, stopper *stop.Stopper) *MembershipCache {
	return &MembershipCache{
		boundAccount: account,
		stopper:      stopper,
	}
}

// userRoleMembership is a mapping of "rolename" -> "with admin option".
type userRoleMembership map[security.SQLUsername]bool

// AuthorizationAccessor for checking authorization (e.g. desc privileges).
type AuthorizationAccessor interface {
	// CheckPrivilege verifies that the user has `privilege` on `descriptor`.
	CheckPrivilegeForUser(
		ctx context.Context, descriptor catalog.Descriptor, privilege privilege.Kind, user security.SQLUsername,
	) error

	// CheckPrivilege verifies that the current user has `privilege` on `descriptor`.
	CheckPrivilege(
		ctx context.Context, descriptor catalog.Descriptor, privilege privilege.Kind,
	) error

	// CheckAnyPrivilege returns nil if user has any privileges at all.
	CheckAnyPrivilege(ctx context.Context, descriptor catalog.Descriptor) error

	// UserHasAdminRole returns tuple of bool and error:
	// (true, nil) means that the user has an admin role (i.e. root or node)
	// (false, nil) means that the user has NO admin role
	// (false, err) means that there was an error running the query on
	// the `system.users` table
	UserHasAdminRole(ctx context.Context, user security.SQLUsername) (bool, error)

	// HasAdminRole checks if the current session's user has admin role.
	HasAdminRole(ctx context.Context) (bool, error)

	// RequireAdminRole is a wrapper on top of HasAdminRole.
	// It errors if HasAdminRole errors or if the user isn't a super-user.
	// Includes the named action in the error message.
	RequireAdminRole(ctx context.Context, action string) error

	// MemberOfWithAdminOption looks up all the roles (direct and indirect) that 'member' is a member
	// of and returns a map of role -> isAdmin.
	MemberOfWithAdminOption(ctx context.Context, member security.SQLUsername) (map[security.SQLUsername]bool, error)

	// HasRoleOption converts the roleoption to its SQL column name and checks if
	// the user belongs to a role where the option has value true. Requires a
	// valid transaction to be open.
	//
	// This check should be done on the version of the privilege that is stored in
	// the role options table. Example: CREATEROLE instead of NOCREATEROLE.
	// NOLOGIN instead of LOGIN.
	HasRoleOption(ctx context.Context, roleOption roleoption.Option) (bool, error)
}

var _ AuthorizationAccessor = &planner{}

// CheckPrivilegeForUser implements the AuthorizationAccessor interface.
// Requires a valid transaction to be open.
func (p *planner) CheckPrivilegeForUser(
	ctx context.Context,
	descriptor catalog.Descriptor,
	privilege privilege.Kind,
	user security.SQLUsername,
) error {
	// Verify that the txn is valid in any case, so that
	// we don't get the risk to say "OK" to root requests
	// with an invalid API usage.
	if p.txn == nil {
		return errors.AssertionFailedf("cannot use CheckPrivilege without a txn")
	}

	// Test whether the object is being audited, and if so, record an
	// audit event. We place this check here to increase the likelihood
	// it will not be forgotten if features are added that access
	// descriptors (since every use of descriptors presumably need a
	// permission check).
	p.maybeAudit(descriptor, privilege)

	privs := descriptor.GetPrivileges()

	// Check if the 'public' pseudo-role has privileges.
	if privs.CheckPrivilege(security.PublicRoleName(), privilege) {
		return nil
	}

	hasPriv, err := p.checkRolePredicate(ctx, user, func(role security.SQLUsername) bool {
		return IsOwner(descriptor, role) || privs.CheckPrivilege(role, privilege)
	})
	if err != nil {
		return err
	}
	if hasPriv {
		return nil
	}
	return pgerror.Newf(pgcode.InsufficientPrivilege,
		"user %s does not have %s privilege on %s %s",
		user, privilege, descriptor.DescriptorType(), descriptor.GetName())
}

// CheckPrivilege implements the AuthorizationAccessor interface.
// Requires a valid transaction to be open.
// TODO(arul): This CheckPrivileges method name is rather deceptive,
// it should be probably be called CheckPrivilegesOrOwnership and return
// a better error.
func (p *planner) CheckPrivilege(
	ctx context.Context, descriptor catalog.Descriptor, privilege privilege.Kind,
) error {
	return p.CheckPrivilegeForUser(ctx, descriptor, privilege, p.User())
}

// CheckGrantOptionsForUser calls PrivilegeDescriptor.CheckGrantOptions, which
// will return an error if a user tries to grant a privilege it does not have
// grant options for. Owners implicitly have all grant options, and also grant
// options are inherited from parent roles.
func (p *planner) CheckGrantOptionsForUser(
	ctx context.Context,
	descriptor catalog.Descriptor,
	privList privilege.List,
	user security.SQLUsername,
	isGrant bool,
) error {
	// Always allow the command to go through if performed by a superuser or the
	// owner of the object
	if isAdmin, err := p.UserHasAdminRole(ctx, user); err != nil {
		return err
	} else if isAdmin {
		return nil
	}

	privs := descriptor.GetPrivileges()
	hasPriv, err := p.checkRolePredicate(ctx, user, func(role security.SQLUsername) bool {
		return IsOwner(descriptor, role) || privs.CheckGrantOptions(role, privList)
	})
	if err != nil {
		return err
	}
	if hasPriv {
		return nil
	}

	code := pgcode.WarningPrivilegeNotGranted
	if !isGrant {
		code = pgcode.WarningPrivilegeNotRevoked
	}
	if privList.Len() > 1 {
		return pgerror.Newf(
			code, "user %s missing WITH GRANT OPTION privilege on one or more of %s",
			user, privList.String(),
		)
	}
	return pgerror.Newf(
		code, "user %s missing WITH GRANT OPTION privilege on %s",
		user, privList.String(),
	)
}

func getOwnerOfDesc(desc catalog.Descriptor) security.SQLUsername {
	// Descriptors created prior to 20.2 do not have owners set.
	owner := desc.GetPrivileges().Owner()
	if owner.Undefined() {
		// If the descriptor is ownerless and the descriptor is part of the system db,
		// node is the owner.
		if catalog.IsSystemDescriptor(desc) {
			owner = security.NodeUserName()
		} else {
			// This check is redundant in this case since admin already has privilege
			// on all non-system objects.
			owner = security.AdminRoleName()
		}
	}
	return owner
}

// IsOwner returns if the role has ownership on the descriptor.
func IsOwner(desc catalog.Descriptor, role security.SQLUsername) bool {
	return role == getOwnerOfDesc(desc)
}

// HasOwnership returns if the role or any role the role is a member of
// has ownership privilege of the desc.
// TODO(richardjcai): SUPERUSER has implicit ownership.
// We do not have SUPERUSER privilege yet but should we consider root a superuser?
func (p *planner) HasOwnership(ctx context.Context, descriptor catalog.Descriptor) (bool, error) {
	user := p.SessionData().User()

	return p.checkRolePredicate(ctx, user, func(role security.SQLUsername) bool {
		return IsOwner(descriptor, role)
	})
}

// checkRolePredicate checks if the predicate is true for the user or
// any roles the user is a member of.
func (p *planner) checkRolePredicate(
	ctx context.Context, user security.SQLUsername, predicate func(role security.SQLUsername) bool,
) (bool, error) {
	if ok := predicate(user); ok {
		return ok, nil
	}
	memberOf, err := p.MemberOfWithAdminOption(ctx, user)
	if err != nil {
		return false, err
	}
	for role := range memberOf {
		if ok := predicate(role); ok {
			return ok, nil
		}
	}
	return false, nil
}

// CheckAnyPrivilege implements the AuthorizationAccessor interface.
// Requires a valid transaction to be open.
func (p *planner) CheckAnyPrivilege(ctx context.Context, descriptor catalog.Descriptor) error {
	// Verify that the txn is valid in any case, so that
	// we don't get the risk to say "OK" to root requests
	// with an invalid API usage.
	if p.txn == nil {
		return errors.AssertionFailedf("cannot use CheckAnyPrivilege without a txn")
	}

	user := p.SessionData().User()

	if user.IsNodeUser() {
		// User "node" has all privileges.
		return nil
	}

	privs := descriptor.GetPrivileges()

	// Check if 'user' itself has privileges.
	if privs.AnyPrivilege(user) {
		return nil
	}

	// Check if 'public' has privileges.
	if privs.AnyPrivilege(security.PublicRoleName()) {
		return nil
	}

	// Expand role memberships.
	memberOf, err := p.MemberOfWithAdminOption(ctx, user)
	if err != nil {
		return err
	}

	// Iterate over the roles that 'user' is a member of. We don't care about the admin option.
	for role := range memberOf {
		if privs.AnyPrivilege(role) {
			return nil
		}
	}

	return pgerror.Newf(pgcode.InsufficientPrivilege,
		"user %s has no privileges on %s %s",
		p.SessionData().User(), descriptor.DescriptorType(), descriptor.GetName())
}

// UserHasAdminRole implements the AuthorizationAccessor interface.
// Requires a valid transaction to be open.
func (p *planner) UserHasAdminRole(ctx context.Context, user security.SQLUsername) (bool, error) {
	if user.Undefined() {
		return false, errors.AssertionFailedf("empty user")
	}
	// Verify that the txn is valid in any case, so that
	// we don't get the risk to say "OK" to root requests
	// with an invalid API usage.
	if p.txn == nil {
		return false, errors.AssertionFailedf("cannot use HasAdminRole without a txn")
	}

	// Check if user is either 'admin', 'root' or 'node'.
	// TODO(knz): planner HasAdminRole has no business authorizing
	// the "node" principal - node should not be issuing SQL queries.
	if user.IsAdminRole() || user.IsRootUser() || user.IsNodeUser() {
		return true, nil
	}

	// Expand role memberships.
	memberOf, err := p.MemberOfWithAdminOption(ctx, user)
	if err != nil {
		return false, err
	}

	// Check is 'user' is a member of role 'admin'.
	if _, ok := memberOf[security.AdminRoleName()]; ok {
		return true, nil
	}

	return false, nil
}

// HasAdminRole implements the AuthorizationAccessor interface.
// Requires a valid transaction to be open.
func (p *planner) HasAdminRole(ctx context.Context) (bool, error) {
	return p.UserHasAdminRole(ctx, p.User())
}

// RequireAdminRole implements the AuthorizationAccessor interface.
// Requires a valid transaction to be open.
func (p *planner) RequireAdminRole(ctx context.Context, action string) error {
	ok, err := p.HasAdminRole(ctx)

	if err != nil {
		return err
	}
	if !ok {
		// raise error if user is not a super-user
		return pgerror.Newf(pgcode.InsufficientPrivilege,
			"only users with the admin role are allowed to %s", action)
	}
	return nil
}

// MemberOfWithAdminOption is a wrapper around the MemberOfWithAdminOption
// method.
func (p *planner) MemberOfWithAdminOption(
	ctx context.Context, member security.SQLUsername,
) (map[security.SQLUsername]bool, error) {
	return MemberOfWithAdminOption(
		ctx,
		p.execCfg,
		p.ExecCfg().InternalExecutor,
		p.Descriptors(),
		p.Txn(),
		member,
	)
}

// MemberOfWithAdminOption looks up all the roles 'member' belongs to (direct and indirect) and
// returns a map of "role" -> "isAdmin".
// The "isAdmin" flag applies to both direct and indirect members.
// Requires a valid transaction to be open.
func MemberOfWithAdminOption(
	ctx context.Context,
	execCfg *ExecutorConfig,
	ie sqlutil.InternalExecutor,
	descsCol *descs.Collection,
	txn *kv.Txn,
	member security.SQLUsername,
) (map[security.SQLUsername]bool, error) {
	if txn == nil {
		return nil, errors.AssertionFailedf("cannot use MemberOfWithAdminoption without a txn")
	}

	roleMembersCache := execCfg.RoleMemberCache

	// Lookup table version.
	_, tableDesc, err := descsCol.GetImmutableTableByName(
		ctx,
		txn,
		&roleMembersTableName,
		tree.ObjectLookupFlagsWithRequired(),
	)
	if err != nil {
		return nil, err
	}

	tableVersion := tableDesc.GetVersion()
	if tableDesc.IsUncommittedVersion() {
		return resolveMemberOfWithAdminOption(ctx, member, ie, txn, useSingleQueryForRoleMembershipCache.Get(execCfg.SV()))
	}

	// Check version and maybe clear cache while holding the mutex.
	// We use a closure here so that we release the lock here, then keep
	// going and re-lock if adding the looked-up entry.
	userMapping, found := func() (userRoleMembership, bool) {
		roleMembersCache.Lock()
		defer roleMembersCache.Unlock()
		if roleMembersCache.tableVersion < tableVersion {
			// If the cache is based on an old table version, then update version and
			// drop the map.
			roleMembersCache.tableVersion = tableVersion
			roleMembersCache.userCache = make(map[security.SQLUsername]userRoleMembership)
			roleMembersCache.boundAccount.Empty(ctx)
		} else if roleMembersCache.tableVersion > tableVersion {
			// If the cache is based on a newer table version, then this transaction
			// should not use the cached data.
			return nil, false
		}
		userMapping, ok := roleMembersCache.userCache[member]
		return userMapping, ok
	}()

	if found {
		// Found: return.
		return userMapping, nil
	}

	// Lookup memberships outside the lock. There will be at most one request
	// in-flight for each user. The role_memberships table version is also part
	// of the request key so that we don't read data from an old version of the
	// table.
	ch, _ := roleMembersCache.populateCacheGroup.DoChan(
		fmt.Sprintf("%s-%d", member.Normalized(), tableVersion),
		func() (interface{}, error) {
			// Use a different context to fetch, so that it isn't possible for
			// one query to timeout and cause all the goroutines that are waiting
			// to get a timeout error.
			ctx, cancel := roleMembersCache.stopper.WithCancelOnQuiesce(
				logtags.WithTags(context.Background(), logtags.FromContext(ctx)))
			defer cancel()
			return resolveMemberOfWithAdminOption(
				ctx, member, ie, txn,
				useSingleQueryForRoleMembershipCache.Get(execCfg.SV()),
			)
		},
	)
	var memberships map[security.SQLUsername]bool
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		memberships = res.Val.(map[security.SQLUsername]bool)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	func() {
		// Update membership if the table version hasn't changed.
		roleMembersCache.Lock()
		defer roleMembersCache.Unlock()
		if roleMembersCache.tableVersion != tableVersion {
			// Table version has changed while we were looking: don't cache the data.
			return
		}

		// Table version remains the same: update map, unlock, return.
		sizeOfEntry := int64(len(member.Normalized()))
		for m := range memberships {
			sizeOfEntry += int64(len(m.Normalized()))
			sizeOfEntry += memsize.Bool
		}
		if err := roleMembersCache.boundAccount.Grow(ctx, sizeOfEntry); err != nil {
			// If there is no memory available to cache the entry, we can still
			// proceed so that the query has a chance to succeed.
			log.Ops.Warningf(ctx, "no memory available to cache role membership info: %v", err)
		} else {
			roleMembersCache.userCache[member] = memberships
		}
	}()
	return memberships, nil
}

var defaultSingleQueryForRoleMembershipCache = util.ConstantWithMetamorphicTestBool(
	"resolve-membership-single-scan-enabled",
	true, /* defaultValue */
)

var useSingleQueryForRoleMembershipCache = settings.RegisterBoolSetting(
	settings.TenantWritable,
	"sql.auth.resolve_membership_single_scan.enabled",
	"determines whether to populate the role membership cache with a single scan",
	defaultSingleQueryForRoleMembershipCache,
).WithPublic()

// resolveMemberOfWithAdminOption performs the actual recursive role membership lookup.
func resolveMemberOfWithAdminOption(
	ctx context.Context,
	member security.SQLUsername,
	ie sqlutil.InternalExecutor,
	txn *kv.Txn,
	singleQuery bool,
) (map[security.SQLUsername]bool, error) {
	ret := map[security.SQLUsername]bool{}
	if singleQuery {
		type membership struct {
			role    security.SQLUsername
			isAdmin bool
		}
		memberToRoles := make(map[security.SQLUsername][]membership)
		if err := forEachRoleMembership(ctx, ie, txn, func(role, member security.SQLUsername, isAdmin bool) error {
			memberToRoles[member] = append(memberToRoles[member], membership{role, isAdmin})
			return nil
		}); err != nil {
			return nil, err
		}

		// Recurse through all roles associated with the member.
		var recurse func(u security.SQLUsername)
		recurse = func(u security.SQLUsername) {
			for _, membership := range memberToRoles[u] {
				// If the parent role was seen before, we still might need to update
				// the isAdmin flag for that role, but there's no need to recurse
				// through the role's ancestry again.
				prev, alreadySeen := ret[membership.role]
				ret[membership.role] = prev || membership.isAdmin
				if !alreadySeen {
					recurse(membership.role)
				}
			}
		}
		recurse(member)
		return ret, nil
	}

	// Keep track of members we looked up.
	visited := map[security.SQLUsername]struct{}{}
	toVisit := []security.SQLUsername{member}
	lookupRolesStmt := `SELECT "role", "isAdmin" FROM system.role_members WHERE "member" = $1`

	for len(toVisit) > 0 {
		// Pop first element.
		m := toVisit[0]
		toVisit = toVisit[1:]
		if _, ok := visited[m]; ok {
			continue
		}
		visited[m] = struct{}{}

		it, err := ie.QueryIterator(
			ctx, "expand-roles", txn, lookupRolesStmt, m.Normalized(),
		)
		if err != nil {
			return nil, err
		}

		var ok bool
		for ok, err = it.Next(ctx); ok; ok, err = it.Next(ctx) {
			row := it.Cur()
			roleName := tree.MustBeDString(row[0])
			isAdmin := row[1].(*tree.DBool)

			// system.role_members stores pre-normalized usernames.
			role := security.MakeSQLUsernameFromPreNormalizedString(string(roleName))
			ret[role] = bool(*isAdmin)

			// We need to expand this role. Let the "pop" worry about already-visited elements.
			toVisit = append(toVisit, role)
		}
		if err != nil {
			return nil, err
		}
	}

	return ret, nil
}

// HasRoleOption implements the AuthorizationAccessor interface.
func (p *planner) HasRoleOption(ctx context.Context, roleOption roleoption.Option) (bool, error) {
	// Verify that the txn is valid in any case, so that
	// we don't get the risk to say "OK" to root requests
	// with an invalid API usage.
	if p.txn == nil {
		return false, errors.AssertionFailedf("cannot use HasRoleOption without a txn")
	}

	user := p.SessionData().User()
	if user.IsRootUser() || user.IsNodeUser() {
		return true, nil
	}

	hasAdmin, err := p.HasAdminRole(ctx)
	if err != nil {
		return false, err
	}
	if hasAdmin {
		// Superusers have all role privileges.
		return true, nil
	}

	hasRolePrivilege, err := p.ExecCfg().InternalExecutor.QueryRowEx(
		ctx, "has-role-option", p.Txn(),
		sessiondata.InternalExecutorOverride{User: security.RootUserName()},
		fmt.Sprintf(
			`SELECT 1 from %s WHERE option = '%s' AND username = $1 LIMIT 1`,
			sessioninit.RoleOptionsTableName, roleOption.String()), user.Normalized())
	if err != nil {
		return false, err
	}

	if len(hasRolePrivilege) != 0 {
		return true, nil
	}

	return false, nil
}

// CheckRoleOption checks if the current user has roleOption and returns an
// insufficient privilege error if the user does not have roleOption.
func (p *planner) CheckRoleOption(ctx context.Context, roleOption roleoption.Option) error {
	hasRoleOption, err := p.HasRoleOption(ctx, roleOption)
	if err != nil {
		return err
	}

	if !hasRoleOption {
		return pgerror.Newf(pgcode.InsufficientPrivilege,
			"user %s does not have %s privilege", p.User(), roleOption)
	}

	return nil
}

// ConnAuditingClusterSettingName is the name of the cluster setting
// for the cluster setting that enables pgwire-level connection audit
// logs.
//
// This name is defined here because it is needed in the telemetry
// counts in SetClusterSetting() and importing pgwire here would
// create a circular dependency.
const ConnAuditingClusterSettingName = "server.auth_log.sql_connections.enabled"

// AuthAuditingClusterSettingName is the name of the cluster setting
// for the cluster setting that enables pgwire-level authentication audit
// logs.
//
// This name is defined here because it is needed in the telemetry
// counts in SetClusterSetting() and importing pgwire here would
// create a circular dependency.
const AuthAuditingClusterSettingName = "server.auth_log.sql_sessions.enabled"

// shouldCheckPublicSchema indicates whether canCreateOnSchema should check
// CREATE privileges for the public schema.
type shouldCheckPublicSchema bool

const (
	checkPublicSchema     shouldCheckPublicSchema = true
	skipCheckPublicSchema shouldCheckPublicSchema = false
)

// canCreateOnSchema returns whether a user has permission to create new objects
// on the specified schema. For `public` schemas, it checks if the user has
// CREATE privileges on the specified dbID. Note that skipCheckPublicSchema may
// be passed to skip this check, since some callers check this separately.
//
// Privileges on temporary schemas are not validated. This is the caller's
// responsibility.
func (p *planner) canCreateOnSchema(
	ctx context.Context,
	schemaID descpb.ID,
	dbID descpb.ID,
	user security.SQLUsername,
	checkPublicSchema shouldCheckPublicSchema,
) error {
	scDesc, err := p.Descriptors().GetImmutableSchemaByID(
		ctx, p.Txn(), schemaID, tree.SchemaLookupFlags{Required: true})
	if err != nil {
		return err
	}

	switch kind := scDesc.SchemaKind(); kind {
	case catalog.SchemaPublic:
		// The public schema is valid to create in if the parent database is.
		if !checkPublicSchema {
			// The caller wishes to skip this check.
			return nil
		}
		_, dbDesc, err := p.Descriptors().GetImmutableDatabaseByID(
			ctx, p.Txn(), dbID, tree.DatabaseLookupFlags{Required: true})
		if err != nil {
			return err
		}
		return p.CheckPrivilegeForUser(ctx, dbDesc, privilege.CREATE, user)
	case catalog.SchemaTemporary:
		// Callers must check whether temporary schemas are valid to create in.
		return nil
	case catalog.SchemaVirtual:
		return pgerror.Newf(pgcode.InsufficientPrivilege,
			"cannot CREATE on schema %s", scDesc.GetName())
	case catalog.SchemaUserDefined:
		return p.CheckPrivilegeForUser(ctx, scDesc, privilege.CREATE, user)
	default:
		panic(errors.AssertionFailedf("unknown schema kind %d", kind))
	}
}

func (p *planner) canResolveDescUnderSchema(
	ctx context.Context, scDesc catalog.SchemaDescriptor, desc catalog.Descriptor,
) error {
	// We can't always resolve temporary schemas by ID (for example in the temporary
	// object cleaner which accesses temporary schemas not in the current session).
	// To avoid an internal error, we just don't check usage on temporary tables.
	if tbl, ok := desc.(catalog.TableDescriptor); ok && tbl.IsTemporary() {
		return nil
	}

	switch kind := scDesc.SchemaKind(); kind {
	case catalog.SchemaPublic, catalog.SchemaTemporary, catalog.SchemaVirtual:
		// Anyone can resolve under temporary, public or virtual schemas.
		return nil
	case catalog.SchemaUserDefined:
		return p.CheckPrivilegeForUser(ctx, scDesc, privilege.USAGE, p.User())
	default:
		panic(errors.AssertionFailedf("unknown schema kind %d", kind))
	}
}

// checkCanAlterToNewOwner checks that the new owner exists and the current user
// has privileges to alter the owner of the object. If the current user is not
// a superuser, it also checks that they are a member of the new owner role.
func (p *planner) checkCanAlterToNewOwner(
	ctx context.Context, desc catalog.MutableDescriptor, newOwner security.SQLUsername,
) error {
	// Make sure the newOwner exists.
	roleExists, err := RoleExists(ctx, p.ExecCfg(), p.Txn(), newOwner)
	if err != nil {
		return err
	}
	if !roleExists {
		return pgerror.Newf(pgcode.UndefinedObject, "role/user %q does not exist", newOwner)
	}

	// If the user is a superuser, skip privilege checks.
	hasAdmin, err := p.HasAdminRole(ctx)
	if err != nil {
		return err
	}
	if hasAdmin {
		return nil
	}

	var objType string
	switch desc.(type) {
	case *typedesc.Mutable:
		objType = "type"
	case *tabledesc.Mutable:
		objType = "table"
	case *schemadesc.Mutable:
		objType = "schema"
	case *dbdesc.Mutable:
		objType = "database"
	default:
		return errors.AssertionFailedf("unknown object descriptor type %v", desc)
	}

	hasOwnership, err := p.HasOwnership(ctx, desc)
	if err != nil {
		return err
	}
	if !hasOwnership {
		return pgerror.Newf(pgcode.InsufficientPrivilege,
			"must be owner of %s %s", tree.Name(objType), tree.Name(desc.GetName()))
	}

	// To alter the owner, you must also be a direct or indirect member of the new
	// owning role.
	if p.User() == newOwner {
		return nil
	}
	memberOf, err := p.MemberOfWithAdminOption(ctx, p.User())
	if err != nil {
		return err
	}
	if _, ok := memberOf[newOwner]; ok {
		return nil
	}
	return pgerror.Newf(pgcode.InsufficientPrivilege, "must be member of role %q", newOwner)
}

// HasOwnershipOnSchema checks if the current user has ownership on the schema.
// For schemas, we cannot always use HasOwnership as not every schema has a
// descriptor.
func (p *planner) HasOwnershipOnSchema(
	ctx context.Context, schemaID descpb.ID, dbID descpb.ID,
) (bool, error) {
	if dbID == keys.SystemDatabaseID {
		// Only the node user has ownership over the system database.
		return p.User().IsNodeUser(), nil
	}
	scDesc, err := p.Descriptors().GetImmutableSchemaByID(
		ctx, p.Txn(), schemaID, tree.SchemaLookupFlags{Required: true},
	)
	if err != nil {
		return false, err
	}

	hasOwnership := false
	switch kind := scDesc.SchemaKind(); kind {
	case catalog.SchemaPublic:
		// admin is the owner of the public schema.
		hasOwnership, err = p.UserHasAdminRole(ctx, p.User())
		if err != nil {
			return false, err
		}
	case catalog.SchemaVirtual:
		// Cannot drop on virtual schemas.
	case catalog.SchemaTemporary, catalog.SchemaUserDefined:
		hasOwnership, err = p.HasOwnership(ctx, scDesc)
		if err != nil {
			return false, err
		}
	default:
		panic(errors.AssertionFailedf("unknown schema kind %d", kind))
	}

	return hasOwnership, nil
}

func (p *planner) HasViewActivityOrViewActivityRedactedRole(ctx context.Context) (bool, error) {
	hasAdmin, err := p.HasAdminRole(ctx)
	if err != nil {
		return hasAdmin, err
	}
	if !hasAdmin {
		hasViewActivity, err := p.HasRoleOption(ctx, roleoption.VIEWACTIVITY)
		if err != nil {
			return hasViewActivity, err
		}
		if !hasViewActivity {
			hasViewActivityRedacted, err := p.HasRoleOption(ctx, roleoption.VIEWACTIVITYREDACTED)
			if err != nil {
				return hasViewActivityRedacted, err
			}
			if !hasViewActivityRedacted {
				return false, nil
			}
		}
	}

	return true, nil
}
