// Copyright 2015 The Cockroach Authors.
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

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/config/zonepb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkeys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/resolver"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/errors"
)

func init() {
	// TODO(marc): we use a hook to avoid a dependency on the sql package. We
	// should probably move keys/protos elsewhere.
	config.ZoneConfigHook = zoneConfigHook
}

var errNoZoneConfigApplies = errors.New("no zone config applies")

// getZoneConfig recursively looks up entries in system.zones until an
// entry that applies to the object with the specified id is
// found. Returns the ID of the matching zone, its zone config, and an
// optional placeholder ID and config if the looked-for ID was a table
// with a zone config specifying only indexes and/or partitions.
//
// This function must be kept in sync with ascendZoneSpecifier.
//
// If getInheritedDefault is true, the direct zone configuration, if it exists, is
// ignored, and the default that would apply if it did not exist is returned instead.
//
// If mayBeTable is true then we will attempt to decode the id into a table
// descriptor in order to find its parent. If false, we'll assume that this
// already is a parent and we'll not decode a descriptor.
func getZoneConfig(
	codec keys.SQLCodec,
	id descpb.ID,
	getKey func(roachpb.Key) (*roachpb.Value, error),
	getInheritedDefault bool,
	mayBeTable bool,
) (descpb.ID, *zonepb.ZoneConfig, descpb.ID, *zonepb.ZoneConfig, error) {
	var placeholder *zonepb.ZoneConfig
	var placeholderID descpb.ID
	if !getInheritedDefault {
		// Look in the zones table.
		if zoneVal, err := getKey(config.MakeZoneKey(codec, id)); err != nil {
			return 0, nil, 0, nil, err
		} else if zoneVal != nil {
			// We found a matching entry.
			var zone zonepb.ZoneConfig
			if err := zoneVal.GetProto(&zone); err != nil {
				return 0, nil, 0, nil, err
			}
			// If the zone isn't a subzone placeholder, we're done.
			if !zone.IsSubzonePlaceholder() {
				return id, &zone, 0, nil, nil
			}
			// If the zone is just a placeholder for subzones, keep recursing
			// up the hierarchy.
			placeholder = &zone
			placeholderID = id
		}
	}

	// No zone config for this ID. We need to figure out if it's a table, so we
	// look up its descriptor.
	if mayBeTable {
		if descVal, err := getKey(catalogkeys.MakeDescMetadataKey(codec, id)); err != nil {
			return 0, nil, 0, nil, err
		} else if descVal != nil {
			var desc descpb.Descriptor
			if err := descVal.GetProto(&desc); err != nil {
				return 0, nil, 0, nil, err
			}
			tableDesc, _, _, _ := descpb.FromDescriptorWithMVCCTimestamp(&desc, descVal.Timestamp)
			if tableDesc != nil {
				// This is a table descriptor. Look up its parent database zone config.
				dbID, zone, _, _, err := getZoneConfig(
					codec,
					tableDesc.ParentID,
					getKey,
					false, /* getInheritedDefault */
					false /* mayBeTable */)
				if err != nil {
					return 0, nil, 0, nil, err
				}
				return dbID, zone, placeholderID, placeholder, nil
			}
		}
	}

	// Retrieve the default zone config, but only as long as that wasn't the ID
	// we were trying to retrieve (avoid infinite recursion).
	if id != keys.RootNamespaceID {
		rootID, zone, _, _, err := getZoneConfig(
			codec,
			keys.RootNamespaceID,
			getKey,
			false, /* getInheritedDefault */
			false /* mayBeTable */)
		if err != nil {
			return 0, nil, 0, nil, err
		}
		return rootID, zone, placeholderID, placeholder, nil
	}

	// No descriptor or not a table.
	return 0, nil, 0, nil, errNoZoneConfigApplies
}

// completeZoneConfig takes a zone config pointer and fills in the
// missing fields by following the chain of inheritance.
// In the worst case, will have to inherit from the default zone config.
// NOTE: This will not work for subzones. To complete subzones, find a complete
// parent zone (index or table) and apply InheritFromParent to it.
func completeZoneConfig(
	cfg *zonepb.ZoneConfig,
	codec keys.SQLCodec,
	id descpb.ID,
	getKey func(roachpb.Key) (*roachpb.Value, error),
) error {
	if cfg.IsComplete() {
		return nil
	}
	// Check to see if its a table. If so, inherit from the database.
	// For all other cases, inherit from the default.
	if descVal, err := getKey(catalogkeys.MakeDescMetadataKey(codec, id)); err != nil {
		return err
	} else if descVal != nil {
		var desc descpb.Descriptor
		if err := descVal.GetProto(&desc); err != nil {
			return err
		}
		tableDesc, _, _, _ := descpb.FromDescriptorWithMVCCTimestamp(&desc, descVal.Timestamp)
		if tableDesc != nil {
			_, dbzone, _, _, err := getZoneConfig(
				codec, tableDesc.ParentID, getKey, false /* getInheritedDefault */, false /* mayBeTable */)
			if err != nil {
				return err
			}
			cfg.InheritFromParent(dbzone)
		}
	}

	// Check if zone is complete. If not, inherit from the default zone config
	if cfg.IsComplete() {
		return nil
	}
	_, defaultZone, _, _, err := getZoneConfig(codec, keys.RootNamespaceID, getKey, false /* getInheritedDefault */, false /* mayBeTable */)
	if err != nil {
		return err
	}
	cfg.InheritFromParent(defaultZone)
	return nil
}

// zoneConfigHook returns the zone config and optional placeholder config for
// the object with id using the cached system config. The returned boolean is
// set to true when the zone config returned can be cached.
//
// zoneConfigHook is a pure function whose only inputs are a system config and
// an object ID. It does not make any external KV calls to look up additional
// state.
func zoneConfigHook(
	cfg *config.SystemConfig, codec keys.SQLCodec, id config.ObjectID,
) (*zonepb.ZoneConfig, *zonepb.ZoneConfig, bool, error) {
	getKey := func(key roachpb.Key) (*roachpb.Value, error) {
		return cfg.GetValue(key), nil
	}
	const mayBeTable = true
	zoneID, zone, _, placeholder, err := getZoneConfig(
		codec, descpb.ID(id), getKey, false /* getInheritedDefault */, mayBeTable)
	if errors.Is(err, errNoZoneConfigApplies) {
		return nil, nil, true, nil
	} else if err != nil {
		return nil, nil, false, err
	}
	if err = completeZoneConfig(zone, codec, zoneID, getKey); err != nil {
		return nil, nil, false, err
	}
	return zone, placeholder, true, nil
}

// GetZoneConfigInTxn looks up the zone and subzone for the specified object ID,
// index, and partition. See the documentation on getZoneConfig for information
// about the getInheritedDefault parameter.
//
// Unlike ZoneConfigHook, GetZoneConfigInTxn does not used a cached system
// config. Instead, it uses the provided txn to make transactionally consistent
// KV lookups.
func GetZoneConfigInTxn(
	ctx context.Context,
	txn *kv.Txn,
	codec keys.SQLCodec,
	id descpb.ID,
	index catalog.Index,
	partition string,
	getInheritedDefault bool,
) (descpb.ID, *zonepb.ZoneConfig, *zonepb.Subzone, error) {
	getKey := func(key roachpb.Key) (*roachpb.Value, error) {
		kv, err := txn.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		return kv.Value, nil
	}
	zoneID, zone, placeholderID, placeholder, err := getZoneConfig(
		codec, id, getKey, getInheritedDefault, true /* mayBeTable */)
	if err != nil {
		return 0, nil, nil, err
	}
	if err = completeZoneConfig(zone, codec, zoneID, getKey); err != nil {
		return 0, nil, nil, err
	}
	var subzone *zonepb.Subzone
	if index != nil {
		indexID := uint32(index.GetID())
		if placeholder != nil {
			if subzone = placeholder.GetSubzone(indexID, partition); subzone != nil {
				if indexSubzone := placeholder.GetSubzone(indexID, ""); indexSubzone != nil {
					subzone.Config.InheritFromParent(&indexSubzone.Config)
				}
				subzone.Config.InheritFromParent(zone)
				return placeholderID, placeholder, subzone, nil
			}
		} else {
			if subzone = zone.GetSubzone(indexID, partition); subzone != nil {
				if indexSubzone := zone.GetSubzone(indexID, ""); indexSubzone != nil {
					subzone.Config.InheritFromParent(&indexSubzone.Config)
				}
				subzone.Config.InheritFromParent(zone)
			}
		}
	}
	return zoneID, zone, subzone, nil
}

// GetHydratedZoneConfigForTenantsRange returns the zone config for RANGE
// TENANTS.
func GetHydratedZoneConfigForTenantsRange(
	ctx context.Context, txn *kv.Txn,
) (*zonepb.ZoneConfig, error) {
	return GetHydratedZoneConfigForNamedZone(
		ctx,
		txn,
		keys.SystemSQLCodec,
		zonepb.TenantsZoneName,
	)
}

// GetHydratedZoneConfigForNamedZone returns a zone config for the given named
// zone. Any missing fields are filled through the RANGE DEFAULT zone config.
func GetHydratedZoneConfigForNamedZone(
	ctx context.Context, txn *kv.Txn, codec keys.SQLCodec, zoneName zonepb.NamedZone,
) (*zonepb.ZoneConfig, error) {
	getKey := func(key roachpb.Key) (*roachpb.Value, error) {
		kv, err := txn.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		return kv.Value, nil
	}
	id, found := zonepb.NamedZones[zoneName]
	if !found {
		return nil, errors.AssertionFailedf("id %d does not belong to a named zone", id)
	}
	zoneID, zone, _, _, err := getZoneConfig(
		codec, descpb.ID(id), getKey, false /* getInheritedDefault */, false, /* mayBeTable */
	)
	if err != nil {
		return nil, err
	}
	if err := completeZoneConfig(zone, codec, zoneID, getKey); err != nil {
		return nil, err
	}
	return zone, nil
}

// GetHydratedZoneConfigForTable returns a fully hydrated zone config for a
// given table ID.
func GetHydratedZoneConfigForTable(
	ctx context.Context, txn *kv.Txn, codec keys.SQLCodec, id descpb.ID,
) (*zonepb.ZoneConfig, error) {
	getKey := func(key roachpb.Key) (*roachpb.Value, error) {
		kv, err := txn.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		return kv.Value, nil
	}
	// TODO(arul): Teach `getZoneConfig` to use a descriptor collection instead of
	// using this getKey function above to do descriptor lookups.
	zoneID, zone, _, placeholder, err := getZoneConfig(
		codec, id, getKey, false /* getInheritedDefault */, true, /* mayBeTable */
	)
	if err != nil {
		return nil, err
	}
	if err := completeZoneConfig(zone, codec, zoneID, getKey); err != nil {
		return nil, err
	}

	// We've completely hydrated the zone config now. The only thing left to do
	// is to do is hydrate the subzones, if applicable.

	// A placeholder config exists only to store subzones, so we copy over that
	// information on the zone config.
	if placeholder != nil {
		// A placeholder config only exists for tables. Furthermore, if it exists,
		// then the zone config (`zone`) above must belong to an object further up
		// in the inheritance chain (such as the database or DEFAULT RANGE). As the
		// subzones field is only defined if the zone config applies to a table, it
		// follows that `zone` must not have any Subzones set on it.
		if len(zone.Subzones) != 0 {
			return nil, errors.AssertionFailedf("placeholder %v exists in conjunction with subzones on zone config %v", *zone, *placeholder)
		}
		zone.Subzones = placeholder.Subzones
		zone.SubzoneSpans = placeholder.SubzoneSpans
	}

	for i, subzone := range zone.Subzones {
		// Check if a zone configuration exists for the index this subzone applies
		// to by passing in a an empty partition below.
		indexSubzone := zone.GetSubzone(subzone.IndexID, "" /* partition  */)
		// Partitions, in terms of the inheritance hierarchy, first inherit from the
		// zone configuration fields on their parent index (if such a zone
		// configuration exists).
		// NB: If the subzone we're dealing with belongs to an index and not a
		// partition, then the call below is a no-op.
		if indexSubzone != nil {
			zone.Subzones[i].Config.InheritFromParent(&indexSubzone.Config)
		}
		// After inheriting from the index's zone configuration, any fields that are
		// left empty must be filled in from the table's zone configuration. Note
		// that the table's zone configuration was fully hydrated above and
		// inheriting from it will result in the subzone config being fully hydrated
		// as well.
		zone.Subzones[i].Config.InheritFromParent(zone)
	}

	return zone, nil
}

// GetHydratedZoneConfigForDatabase returns a fully hydrated zone config for a
// given database ID.
func GetHydratedZoneConfigForDatabase(
	ctx context.Context, txn *kv.Txn, codec keys.SQLCodec, id descpb.ID,
) (*zonepb.ZoneConfig, error) {
	getKey := func(key roachpb.Key) (*roachpb.Value, error) {
		kv, err := txn.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		return kv.Value, nil
	}
	zoneID, zone, _, _, err := getZoneConfig(
		codec, id, getKey, false /* getInheritedDefault */, false, /* mayBeTable */
	)
	if err != nil {
		return nil, err
	}
	if err := completeZoneConfig(zone, codec, zoneID, getKey); err != nil {
		return nil, err
	}

	return zone, nil
}

func zoneSpecifierNotFoundError(zs tree.ZoneSpecifier) error {
	if zs.NamedZone != "" {
		return pgerror.Newf(
			pgcode.InvalidCatalogName, "zone %q does not exist", zs.NamedZone)
	} else if zs.Database != "" {
		return sqlerrors.NewUndefinedDatabaseError(string(zs.Database))
	} else {
		return sqlerrors.NewUndefinedRelationError(&zs.TableOrIndex)
	}
}

// resolveTableForZone ensures that the table part of the zone
// specifier is resolved (or resolvable) and, if the zone specifier
// points to an index, that the index name is expanded to a valid
// table.
// Returns res = nil if the zone specifier is not for a table or index.
func (p *planner) resolveTableForZone(
	ctx context.Context, zs *tree.ZoneSpecifier,
) (res catalog.TableDescriptor, err error) {
	if zs.TargetsIndex() {
		var mutRes *tabledesc.Mutable
		_, mutRes, err = expandMutableIndexName(ctx, p, &zs.TableOrIndex, true /* requireTable */)
		if mutRes != nil {
			res = mutRes
		}
	} else if zs.TargetsTable() {
		var immutRes catalog.TableDescriptor
		p.runWithOptions(resolveFlags{skipCache: true}, func() {
			flags := tree.ObjectLookupFlagsWithRequiredTableKind(tree.ResolveAnyTableKind)
			flags.IncludeOffline = true
			_, immutRes, err = resolver.ResolveExistingTableObject(ctx, p, &zs.TableOrIndex.Table, flags)
		})
		if err != nil {
			return nil, err
		} else if immutRes != nil {
			res = immutRes
		}
	}
	return res, err
}

// resolveZone resolves a zone specifier to a zone ID.  If the zone
// specifier points to a table, index or partition, the table part
// must be properly normalized already. It is the caller's
// responsibility to do this using e.g .resolveTableForZone().
func resolveZone(
	ctx context.Context,
	txn *kv.Txn,
	col *descs.Collection,
	zs *tree.ZoneSpecifier,
	version clusterversion.Handle,
) (descpb.ID, error) {
	errMissingKey := errors.New("missing key")
	id, err := zonepb.ResolveZoneSpecifier(ctx, zs,
		func(parentID uint32, schemaID uint32, name string) (uint32, error) {
			id, err := col.Direct().LookupObjectID(ctx, txn, descpb.ID(parentID), descpb.ID(schemaID), name)
			if err != nil {
				return 0, err
			}
			if id == descpb.InvalidID {
				return 0, errMissingKey
			}
			return uint32(id), nil
		},
		version,
	)
	if err != nil {
		if errors.Is(err, errMissingKey) {
			return 0, zoneSpecifierNotFoundError(*zs)
		}
		return 0, err
	}
	return descpb.ID(id), nil
}

func resolveSubzone(
	zs *tree.ZoneSpecifier, table catalog.TableDescriptor,
) (catalog.Index, string, error) {
	if !zs.TargetsTable() || zs.TableOrIndex.Index == "" && zs.Partition == "" {
		return nil, "", nil
	}

	indexName := string(zs.TableOrIndex.Index)
	var index catalog.Index
	if indexName == "" {
		index = table.GetPrimaryIndex()
		indexName = index.GetName()
	} else {
		var err error
		index, err = table.FindIndexWithName(indexName)
		if err != nil {
			return nil, "", err
		}
	}

	partitionName := string(zs.Partition)
	if partitionName != "" {
		if index.GetPartitioning().FindPartitionByName(partitionName) == nil {
			return nil, "", fmt.Errorf("partition %q does not exist on index %q", partitionName, indexName)
		}
	}

	return index, partitionName, nil
}

func prepareRemovedPartitionZoneConfigs(
	ctx context.Context,
	txn *kv.Txn,
	tableDesc catalog.TableDescriptor,
	indexID descpb.IndexID,
	oldPart catalog.Partitioning,
	newPart catalog.Partitioning,
	execCfg *ExecutorConfig,
) (*zoneConfigUpdate, error) {
	newNames := map[string]struct{}{}
	_ = newPart.ForEachPartitionName(func(newName string) error {
		newNames[newName] = struct{}{}
		return nil
	})
	removedNames := make([]string, 0, len(newNames))
	_ = oldPart.ForEachPartitionName(func(oldName string) error {
		if _, exists := newNames[oldName]; !exists {
			removedNames = append(removedNames, oldName)
		}
		return nil
	})
	if len(removedNames) == 0 {
		return nil, nil
	}
	zone, err := getZoneConfigRaw(ctx, txn, execCfg.Codec, execCfg.Settings, tableDesc.GetID())
	if err != nil {
		return nil, err
	} else if zone == nil {
		zone = zonepb.NewZoneConfig()
	}
	for _, n := range removedNames {
		zone.DeleteSubzone(uint32(indexID), n)
	}
	return prepareZoneConfigWrites(
		ctx, execCfg, tableDesc.GetID(), tableDesc, zone, false, /* hasNewSubzones */
	)
}

func deleteRemovedPartitionZoneConfigs(
	ctx context.Context,
	txn *kv.Txn,
	tableDesc catalog.TableDescriptor,
	indexID descpb.IndexID,
	oldPart catalog.Partitioning,
	newPart catalog.Partitioning,
	execCfg *ExecutorConfig,
) error {
	update, err := prepareRemovedPartitionZoneConfigs(
		ctx, txn, tableDesc, indexID, oldPart, newPart, execCfg,
	)
	if update == nil || err != nil {
		return err
	}
	_, err = writeZoneConfigUpdate(ctx, txn, execCfg, update)
	return err
}
