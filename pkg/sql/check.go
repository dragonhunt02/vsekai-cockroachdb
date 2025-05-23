// Copyright 2016 The Cockroach Authors.
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
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemaexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	pbtypes "github.com/gogo/protobuf/types"
)

// validateCheckExpr verifies that the given CHECK expression returns true
// for all the rows in the table.
//
// It operates entirely on the current goroutine and is thus able to
// reuse an existing client.Txn safely.
func validateCheckExpr(
	ctx context.Context,
	semaCtx *tree.SemaContext,
	sessionData *sessiondata.SessionData,
	exprStr string,
	tableDesc *tabledesc.Mutable,
	ie sqlutil.InternalExecutor,
	txn *kv.Txn,
) error {
	expr, err := schemaexpr.FormatExprForDisplay(ctx, tableDesc, exprStr, semaCtx, sessionData, tree.FmtParsable)
	if err != nil {
		return err
	}
	colSelectors := tabledesc.ColumnsSelectors(tableDesc.AccessibleColumns())
	columns := tree.AsStringWithFlags(&colSelectors, tree.FmtSerializable)
	queryStr := fmt.Sprintf(`SELECT %s FROM [%d AS t] WHERE NOT (%s) LIMIT 1`, columns, tableDesc.GetID(), exprStr)
	log.Infof(ctx, "validating check constraint %q with query %q", expr, queryStr)

	rows, err := ie.QueryRow(ctx, "validate check constraint", txn, queryStr)
	if err != nil {
		return err
	}
	if rows.Len() > 0 {
		return pgerror.Newf(pgcode.CheckViolation,
			"validation of CHECK %q failed on row: %s",
			expr, labeledRowValues(tableDesc.AccessibleColumns(), rows))
	}
	return nil
}

// matchFullUnacceptableKeyQuery generates and returns a query for rows that are
// disallowed given the specified MATCH FULL composite FK reference, i.e., rows
// in the referencing table where the key contains both null and non-null
// values.
//
// For example, a FK constraint on columns (a_id, b_id) with an index c_id on
// the table "child" would require the following query:
//
// SELECT s.a_id, s.b_id, s.pk1, s.pk2 FROM child@c_idx
// WHERE
//   (a_id IS NULL OR b_id IS NULL) AND (a_id IS NOT NULL OR b_id IS NOT NULL)
// LIMIT 1;
func matchFullUnacceptableKeyQuery(
	srcTbl catalog.TableDescriptor, fk *descpb.ForeignKeyConstraint, limitResults bool,
) (sql string, colNames []string, _ error) {
	nCols := len(fk.OriginColumnIDs)
	srcCols := make([]string, nCols)
	srcNullExistsClause := make([]string, nCols)
	srcNotNullExistsClause := make([]string, nCols)

	returnedCols := srcCols
	for i := 0; i < nCols; i++ {
		col, err := srcTbl.FindColumnWithID(fk.OriginColumnIDs[i])
		if err != nil {
			return "", nil, err
		}
		srcCols[i] = tree.NameString(col.GetName())
		srcNullExistsClause[i] = fmt.Sprintf("%s IS NULL", srcCols[i])
		srcNotNullExistsClause[i] = fmt.Sprintf("%s IS NOT NULL", srcCols[i])
	}

	for i := 0; i < srcTbl.GetPrimaryIndex().NumKeyColumns(); i++ {
		id := srcTbl.GetPrimaryIndex().GetKeyColumnID(i)
		alreadyPresent := false
		for _, otherID := range fk.OriginColumnIDs {
			if id == otherID {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			col, err := tabledesc.FindPublicColumnWithID(srcTbl, id)
			if err != nil {
				return "", nil, err
			}
			returnedCols = append(returnedCols, col.GetName())
		}
	}

	limit := ""
	if limitResults {
		limit = " LIMIT 1"
	}
	return fmt.Sprintf(
		`SELECT %[1]s FROM [%[2]d AS tbl] WHERE (%[3]s) AND (%[4]s) %[5]s`,
		strings.Join(returnedCols, ","),              // 1
		srcTbl.GetID(),                               // 2
		strings.Join(srcNullExistsClause, " OR "),    // 3
		strings.Join(srcNotNullExistsClause, " OR "), // 4
		limit, // 5
	), returnedCols, nil
}

// nonMatchingRowQuery generates and returns a query for rows that violate the
// specified FK constraint, i.e., rows in the referencing table with no matching
// key in the referenced table. Rows in the referencing table with any null
// values in the key are excluded from matching (for both MATCH FULL and MATCH
// SIMPLE).
//
// For example, a FK constraint on columns (a_id, b_id) on the table "child",
// referencing columns (a, b) on the table "parent", would require the following
// query:
//
// SELECT s.a_id, s.b_id, s.rowid
//  FROM (
//        SELECT a_id, b_id, rowid
//          FROM [<ID of child> AS src]@{IGNORE_FOREIGN_KEYS}
//         WHERE a_id IS NOT NULL AND b_id IS NOT NULL
//       ) AS s
//       LEFT JOIN [<id of parent> AS target] AS t ON s.a_id = t.a AND s.b_id = t.b
// WHERE t.a IS NULL
// LIMIT 1  -- if limitResults is set
//
// TODO(radu): change this to a query which executes as an anti-join when we
// remove the heuristic planner.
func nonMatchingRowQuery(
	srcTbl catalog.TableDescriptor,
	fk *descpb.ForeignKeyConstraint,
	targetTbl catalog.TableDescriptor,
	limitResults bool,
) (sql string, originColNames []string, _ error) {
	originColNames, err := srcTbl.NamesForColumnIDs(fk.OriginColumnIDs)
	if err != nil {
		return "", nil, err
	}
	// Get primary key columns not included in the FK
	for i := 0; i < srcTbl.GetPrimaryIndex().NumKeyColumns(); i++ {
		pkColID := srcTbl.GetPrimaryIndex().GetKeyColumnID(i)
		found := false
		for _, id := range fk.OriginColumnIDs {
			if pkColID == id {
				found = true
				break
			}
		}
		if !found {
			column, err := tabledesc.FindPublicColumnWithID(srcTbl, pkColID)
			if err != nil {
				return "", nil, err
			}
			originColNames = append(originColNames, column.GetName())
		}
	}
	srcCols := make([]string, len(originColNames))
	qualifiedSrcCols := make([]string, len(originColNames))
	for i, n := range originColNames {
		srcCols[i] = tree.NameString(n)
		// s is the table alias used in the query.
		qualifiedSrcCols[i] = fmt.Sprintf("s.%s", srcCols[i])
	}

	referencedColNames, err := targetTbl.NamesForColumnIDs(fk.ReferencedColumnIDs)
	if err != nil {
		return "", nil, err
	}
	nCols := len(fk.OriginColumnIDs)
	srcWhere := make([]string, nCols)
	targetCols := make([]string, nCols)
	on := make([]string, nCols)

	for i := 0; i < nCols; i++ {
		// s and t are table aliases used in the query
		srcWhere[i] = fmt.Sprintf("%s IS NOT NULL", srcCols[i])
		targetCols[i] = fmt.Sprintf("t.%s", tree.NameString(referencedColNames[i]))
		on[i] = fmt.Sprintf("%s = %s", qualifiedSrcCols[i], targetCols[i])
	}

	limit := ""
	if limitResults {
		limit = " LIMIT 1"
	}
	return fmt.Sprintf(
		`SELECT %[1]s FROM 
		  (SELECT %[2]s FROM [%[3]d AS src]@{IGNORE_FOREIGN_KEYS} WHERE %[4]s) AS s
			LEFT OUTER JOIN
			[%[5]d AS target] AS t
			ON %[6]s
		 WHERE %[7]s IS NULL %[8]s`,
		strings.Join(qualifiedSrcCols, ", "), // 1
		strings.Join(srcCols, ", "),          // 2
		srcTbl.GetID(),                       // 3
		strings.Join(srcWhere, " AND "),      // 4
		targetTbl.GetID(),                    // 5
		strings.Join(on, " AND "),            // 6
		// Sufficient to check the first column to see whether there was no matching row
		targetCols[0], // 7
		limit,         // 8
	), originColNames, nil
}

// validateForeignKey verifies that all the rows in the srcTable
// have a matching row in their referenced table.
//
// It operates entirely on the current goroutine and is thus able to
// reuse an existing kv.Txn safely.
func validateForeignKey(
	ctx context.Context,
	srcTable *tabledesc.Mutable,
	targetTable catalog.TableDescriptor,
	fk *descpb.ForeignKeyConstraint,
	ie sqlutil.InternalExecutor,
	txn *kv.Txn,
) error {
	nCols := len(fk.OriginColumnIDs)

	referencedColumnNames, err := targetTable.NamesForColumnIDs(fk.ReferencedColumnIDs)
	if err != nil {
		return err
	}

	// For MATCH FULL FKs, first check whether any disallowed keys containing both
	// null and non-null values exist.
	// (The matching options only matter for FKs with more than one column.)
	if nCols > 1 && fk.Match == descpb.ForeignKeyReference_FULL {
		query, colNames, err := matchFullUnacceptableKeyQuery(
			srcTable, fk, true, /* limitResults */
		)
		if err != nil {
			return err
		}

		log.Infof(ctx, "validating MATCH FULL FK %q (%q [%v] -> %q [%v]) with query %q",
			fk.Name,
			srcTable.Name, colNames,
			targetTable.GetName(), referencedColumnNames,
			query,
		)

		values, err := ie.QueryRowEx(ctx, "validate foreign key constraint",
			txn, sessiondata.NodeUserSessionDataOverride, query)
		if err != nil {
			return err
		}
		if values.Len() > 0 {
			return pgerror.WithConstraintName(pgerror.Newf(pgcode.ForeignKeyViolation,
				"foreign key violation: MATCH FULL does not allow mixing of null and nonnull values %s for %s",
				formatValues(colNames, values), fk.Name,
			), fk.Name)
		}
	}
	query, colNames, err := nonMatchingRowQuery(
		srcTable, fk, targetTable,
		true, /* limitResults */
	)
	if err != nil {
		return err
	}

	log.Infof(ctx, "validating FK %q (%q [%v] -> %q [%v]) with query %q",
		fk.Name,
		srcTable.Name, colNames, targetTable.GetName(), referencedColumnNames,
		query,
	)

	values, err := ie.QueryRowEx(ctx, "validate fk constraint", txn,
		sessiondata.NodeUserSessionDataOverride, query)
	if err != nil {
		return err
	}
	if values.Len() > 0 {
		return pgerror.WithConstraintName(pgerror.Newf(pgcode.ForeignKeyViolation,
			"foreign key violation: %q row %s has no match in %q",
			srcTable.Name, formatValues(colNames, values), targetTable.GetName()), fk.Name)
	}
	return nil
}

// duplicateRowQuery generates and returns a query for column values that
// violate the specified unique constraint. Rows in the table with any null
// values in the key are excluded from matching.
//
// For example, a unique constraint on columns (a, b) on the table "tbl" would
// require the following query:
//
// SELECT a, b
// FROM tbl
// WHERE a IS NOT NULL AND b IS NOT NULL
// GROUP BY a, b
// HAVING count(*) > 1
// LIMIT 1  -- if limitResults is set
//
// The pred argument is a partial unique constraint predicate, which filters the
// subset of rows that are guaranteed unique by the constraint. If the unique
// constraint is not partial, pred should be empty.
func duplicateRowQuery(
	srcTbl catalog.TableDescriptor, columnIDs []descpb.ColumnID, pred string, limitResults bool,
) (sql string, colNames []string, _ error) {
	colNames, err := srcTbl.NamesForColumnIDs(columnIDs)
	if err != nil {
		return "", nil, err
	}

	srcCols := make([]string, len(colNames))
	for i, n := range colNames {
		srcCols[i] = tree.NameString(n)
	}

	// There will be an expression in the WHERE clause for each of the columns,
	// and possibly one for pred.
	srcWhere := make([]string, 0, len(srcCols)+1)
	for i := range srcCols {
		srcWhere = append(srcWhere, fmt.Sprintf("%s IS NOT NULL", srcCols[i]))
	}

	// Wrap the predicate in parentheses.
	if pred != "" {
		srcWhere = append(srcWhere, fmt.Sprintf("(%s)", pred))
	}

	limit := ""
	if limitResults {
		limit = " LIMIT 1"
	}
	return fmt.Sprintf(
		`SELECT %[1]s FROM [%[2]d AS tbl] WHERE %[3]s GROUP BY %[1]s HAVING count(*) > 1 %[4]s`,
		strings.Join(srcCols, ", "),     // 1
		srcTbl.GetID(),                  // 2
		strings.Join(srcWhere, " AND "), // 3
		limit,                           // 4
	), colNames, nil
}

// RevalidateUniqueConstraintsInCurrentDB verifies that all unique constraints
// defined on tables in the current database are valid. In other words, it
// verifies that for every table in the database with one or more unique
// constraints, all rows in the table have unique values for every unique
// constraint defined on the table.
func (p *planner) RevalidateUniqueConstraintsInCurrentDB(ctx context.Context) error {
	dbName := p.CurrentDatabase()
	log.Infof(ctx, "validating unique constraints in database %s", dbName)
	db, err := p.Descriptors().GetImmutableDatabaseByName(
		ctx, p.Txn(), dbName, tree.DatabaseLookupFlags{Required: true},
	)
	if err != nil {
		return err
	}
	tableDescs, err := p.Descriptors().GetAllTableDescriptorsInDatabase(ctx, p.Txn(), db.GetID())
	if err != nil {
		return err
	}

	for _, tableDesc := range tableDescs {
		if err = RevalidateUniqueConstraintsInTable(
			ctx, p.Txn(), p.User(), p.ExecCfg().InternalExecutor, tableDesc,
		); err != nil {
			return err
		}
	}
	return nil
}

// RevalidateUniqueConstraintsInTable verifies that all unique constraints
// defined on the given table are valid. In other words, it verifies that all
// rows in the table have unique values for every unique constraint defined on
// the table.
func (p *planner) RevalidateUniqueConstraintsInTable(ctx context.Context, tableID int) error {
	tableDesc, err := p.Descriptors().GetImmutableTableByID(
		ctx,
		p.Txn(),
		descpb.ID(tableID),
		tree.ObjectLookupFlagsWithRequired(),
	)
	if err != nil {
		return err
	}
	return RevalidateUniqueConstraintsInTable(
		ctx, p.Txn(), p.User(), p.ExecCfg().InternalExecutor, tableDesc,
	)
}

// RevalidateUniqueConstraint verifies that the given unique constraint on the
// given table is valid. In other words, it verifies that all rows in the
// table have unique values for the columns in the constraint. Returns an
// error if validation fails or if constraintName is not actually a unique
// constraint on the table.
func (p *planner) RevalidateUniqueConstraint(
	ctx context.Context, tableID int, constraintName string,
) error {
	tableDesc, err := p.Descriptors().GetImmutableTableByID(
		ctx,
		p.Txn(),
		descpb.ID(tableID),
		tree.ObjectLookupFlagsWithRequired(),
	)
	if err != nil {
		return err
	}

	// Check implicitly partitioned UNIQUE indexes.
	for _, index := range tableDesc.ActiveIndexes() {
		if index.GetName() == constraintName {
			if !index.IsUnique() {
				return errors.Newf("%s is not a unique constraint", constraintName)
			}
			if index.GetPartitioning().NumImplicitColumns() > 0 {
				return validateUniqueConstraint(
					ctx,
					tableDesc,
					index.GetName(),
					index.IndexDesc().KeyColumnIDs[index.GetPartitioning().NumImplicitColumns():],
					index.GetPredicate(),
					p.ExecCfg().InternalExecutor,
					p.Txn(),
					p.User(),
					true, /* preExisting */
				)
			}
			// We found the unique index but we don't need to bother validating it.
			return nil
		}
	}

	// Check UNIQUE WITHOUT INDEX constraints.
	for _, uc := range tableDesc.GetUniqueWithoutIndexConstraints() {
		if uc.Name == constraintName {
			return validateUniqueConstraint(
				ctx,
				tableDesc,
				uc.Name,
				uc.ColumnIDs,
				uc.Predicate,
				p.ExecCfg().InternalExecutor,
				p.Txn(),
				p.User(),
				true, /* preExisting */
			)
		}
	}

	return errors.Newf("unique constraint %s does not exist", constraintName)
}

// HasVirtualUniqueConstraints returns true if the table has one or more
// constraints that are validated by RevalidateUniqueConstraintsInTable.
func HasVirtualUniqueConstraints(tableDesc catalog.TableDescriptor) bool {
	for _, index := range tableDesc.ActiveIndexes() {
		if index.IsUnique() && index.GetPartitioning().NumImplicitColumns() > 0 {
			return true
		}
	}
	for _, uc := range tableDesc.GetUniqueWithoutIndexConstraints() {
		if uc.Validity == descpb.ConstraintValidity_Validated {
			return true
		}
	}
	return false
}

// RevalidateUniqueConstraintsInTable verifies that all unique constraints
// defined on the given table are valid. In other words, it verifies that all
// rows in the table have unique values for every unique constraint defined on
// the table.
//
// Note that we only need to validate UNIQUE constraints that are not already
// enforced by an index. This includes implicitly partitioned UNIQUE indexes
// and UNIQUE WITHOUT INDEX constraints.
func RevalidateUniqueConstraintsInTable(
	ctx context.Context,
	txn *kv.Txn,
	user security.SQLUsername,
	ie sqlutil.InternalExecutor,
	tableDesc catalog.TableDescriptor,
) error {
	// Check implicitly partitioned UNIQUE indexes.
	for _, index := range tableDesc.ActiveIndexes() {
		if index.IsUnique() && index.GetPartitioning().NumImplicitColumns() > 0 {
			if err := validateUniqueConstraint(
				ctx,
				tableDesc,
				index.GetName(),
				index.IndexDesc().KeyColumnIDs[index.GetPartitioning().NumImplicitColumns():],
				index.GetPredicate(),
				ie,
				txn,
				user,
				true, /* preExisting */
			); err != nil {
				log.Errorf(ctx, "validation of unique constraints failed for table %s: %s", tableDesc.GetName(), err)
				return errors.Wrapf(err, "for table %s", tableDesc.GetName())
			}
		}
	}

	// Check UNIQUE WITHOUT INDEX constraints.
	for _, uc := range tableDesc.GetUniqueWithoutIndexConstraints() {
		if uc.Validity == descpb.ConstraintValidity_Validated {
			if err := validateUniqueConstraint(
				ctx,
				tableDesc,
				uc.Name,
				uc.ColumnIDs,
				uc.Predicate,
				ie,
				txn,
				user,
				true, /* preExisting */
			); err != nil {
				log.Errorf(ctx, "validation of unique constraints failed for table %s: %s", tableDesc.GetName(), err)
				return errors.Wrapf(err, "for table %s", tableDesc.GetName())
			}
		}
	}

	log.Infof(ctx, "validated all unique constraints in table %s", tableDesc.GetName())
	return nil
}

// validateUniqueConstraint verifies that all the rows in the srcTable
// have unique values for the given columns.
//
// It operates entirely on the current goroutine and is thus able to
// reuse an existing kv.Txn safely.
//
// preExisting indicates whether this constraint already exists, and therefore
// informs the error message that gets produced.
func validateUniqueConstraint(
	ctx context.Context,
	srcTable catalog.TableDescriptor,
	constraintName string,
	columnIDs []descpb.ColumnID,
	pred string,
	ie sqlutil.InternalExecutor,
	txn *kv.Txn,
	user security.SQLUsername,
	preExisting bool,
) error {
	query, colNames, err := duplicateRowQuery(
		srcTable, columnIDs, pred, true, /* limitResults */
	)
	if err != nil {
		return err
	}

	log.Infof(ctx, "validating unique constraint %q (%q [%v]) with query %q",
		constraintName,
		srcTable.GetName(),
		colNames,
		query,
	)

	sessionDataOverride := sessiondata.NoSessionDataOverride
	sessionDataOverride.User = user
	values, err := ie.QueryRowEx(ctx, "validate unique constraint", txn, sessionDataOverride, query)
	if err != nil {
		return err
	}
	if values.Len() > 0 {
		valuesStr := make([]string, len(values))
		for i := range values {
			valuesStr[i] = values[i].String()
		}
		// Note: this error message mirrors the message produced by Postgres
		// when it fails to add a unique index due to duplicated keys.
		errMsg := "could not create unique constraint"
		if preExisting {
			errMsg = "failed to validate unique constraint"
		}
		return errors.WithDetail(
			pgerror.WithConstraintName(
				pgerror.Newf(
					pgcode.UniqueViolation, "%s %q", errMsg, constraintName,
				),
				constraintName,
			),
			fmt.Sprintf(
				"Key (%s)=(%s) is duplicated.", strings.Join(colNames, ","), strings.Join(valuesStr, ","),
			),
		)
	}
	return nil
}

// ValidateTTLScheduledJobsInCurrentDB is part of the EvalPlanner interface.
func (p *planner) ValidateTTLScheduledJobsInCurrentDB(ctx context.Context) error {
	dbName := p.CurrentDatabase()
	log.Infof(ctx, "validating scheduled jobs in database %s", dbName)
	db, err := p.Descriptors().GetImmutableDatabaseByName(
		ctx, p.Txn(), dbName, tree.DatabaseLookupFlags{Required: true},
	)
	if err != nil {
		return err
	}
	tableDescs, err := p.Descriptors().GetAllTableDescriptorsInDatabase(ctx, p.Txn(), db.GetID())
	if err != nil {
		return err
	}

	for _, tableDesc := range tableDescs {
		if err = p.validateTTLScheduledJobInTable(ctx, tableDesc); err != nil {
			return err
		}
	}
	return nil
}

var invalidTableTTLScheduledJobError = errors.Newf("invalid scheduled job for table")

// validateTTLScheduledJobsInCurrentDB is part of the EvalPlanner interface.
func (p *planner) validateTTLScheduledJobInTable(
	ctx context.Context, tableDesc catalog.TableDescriptor,
) error {
	if !tableDesc.HasRowLevelTTL() {
		return nil
	}
	ttl := tableDesc.GetRowLevelTTL()

	execCfg := p.ExecCfg()
	env := JobSchedulerEnv(execCfg)

	wrapError := func(origErr error) error {
		return errors.WithHintf(
			errors.Mark(origErr, invalidTableTTLScheduledJobError),
			`use crdb_internal.repair_ttl_table_scheduled_job(%d) to repair the missing job`,
			tableDesc.GetID(),
		)
	}

	sj, err := jobs.LoadScheduledJob(
		ctx,
		env,
		ttl.ScheduleID,
		execCfg.InternalExecutor,
		p.txn,
	)
	if err != nil {
		if jobs.HasScheduledJobNotFoundError(err) {
			return wrapError(
				pgerror.Newf(
					pgcode.Internal,
					"table id %d maps to a non-existent schedule id %d",
					tableDesc.GetID(),
					ttl.ScheduleID,
				),
			)
		}
		return errors.Wrapf(err, "error fetching schedule id %d for table id %d", ttl.ScheduleID, tableDesc.GetID())
	}

	var args catpb.ScheduledRowLevelTTLArgs
	if err := pbtypes.UnmarshalAny(sj.ExecutionArgs().Args, &args); err != nil {
		return wrapError(
			pgerror.Wrapf(
				err,
				pgcode.Internal,
				"error unmarshalling scheduled jobs args for table id %d, schedule id %d",
				tableDesc.GetID(),
				ttl.ScheduleID,
			),
		)
	}

	if args.TableID != tableDesc.GetID() {
		return wrapError(
			pgerror.Newf(
				pgcode.Internal,
				"schedule id %d points to table id %d instead of table id %d",
				ttl.ScheduleID,
				args.TableID,
				tableDesc.GetID(),
			),
		)
	}

	return nil
}

// RepairTTLScheduledJobForTable is part of the EvalPlanner interface.
func (p *planner) RepairTTLScheduledJobForTable(ctx context.Context, tableID int64) error {
	tableDesc, err := p.Descriptors().GetMutableTableByID(ctx, p.txn, descpb.ID(tableID), tree.ObjectLookupFlagsWithRequired())
	if err != nil {
		return err
	}
	validateErr := p.validateTTLScheduledJobInTable(ctx, tableDesc)
	if validateErr == nil {
		return nil
	}
	if !errors.HasType(validateErr, invalidTableTTLScheduledJobError) {
		return errors.Wrap(validateErr, "error validating TTL on table")
	}
	sj, err := CreateRowLevelTTLScheduledJob(
		ctx,
		p.ExecCfg(),
		p.txn,
		p.User(),
		tableDesc.GetID(),
		tableDesc.GetRowLevelTTL(),
	)
	if err != nil {
		return err
	}
	tableDesc.RowLevelTTL.ScheduleID = sj.ScheduleID()
	return p.Descriptors().WriteDesc(
		ctx, false /* kvTrace */, tableDesc, p.txn,
	)
}

func formatValues(colNames []string, values tree.Datums) string {
	var pairs bytes.Buffer
	for i := range values {
		if i > 0 {
			pairs.WriteString(", ")
		}
		pairs.WriteString(fmt.Sprintf("%s=%v", colNames[i], values[i]))
	}
	return pairs.String()
}

// checkSet contains a subset of checks, as ordinals into
// immutable.ActiveChecks. These checks have boolean columns
// produced as input to mutations, indicating the result of evaluating the
// check.
//
// It is allowed to check only a subset of the active checks (the optimizer
// could in principle determine that some checks can't fail because they
// statically evaluate to true for the entire input).
type checkSet = util.FastIntSet

// When executing mutations, we calculate a boolean column for each check
// indicating if the check passed. This function verifies that each result is
// true or null.
//
// It is allowed to check only a subset of the active checks (for some, we could
// determine that they can't fail because they statically evaluate to true for
// the entire input); checkOrds contains the set of checks for which we have
// values, as ordinals into ActiveChecks(). There must be exactly one value in
// checkVals for each element in checkSet.
func checkMutationInput(
	ctx context.Context,
	semaCtx *tree.SemaContext,
	sessionData *sessiondata.SessionData,
	tabDesc catalog.TableDescriptor,
	checkOrds checkSet,
	checkVals tree.Datums,
) error {
	if len(checkVals) < checkOrds.Len() {
		return errors.AssertionFailedf(
			"mismatched check constraint columns: expected %d, got %d", checkOrds.Len(), len(checkVals))
	}

	checks := tabDesc.ActiveChecks()
	colIdx := 0
	for i := range checks {
		if !checkOrds.Contains(i) {
			continue
		}

		if res, err := tree.GetBool(checkVals[colIdx]); err != nil {
			return err
		} else if !res && checkVals[colIdx] != tree.DNull {
			// Failed to satisfy CHECK constraint, so unwrap the serialized
			// check expression to display to the user.
			expr, err := schemaexpr.FormatExprForDisplay(
				ctx, tabDesc, checks[i].Expr, semaCtx, sessionData, tree.FmtParsable,
			)
			if err != nil {
				// If we ran into an error trying to read the check constraint, wrap it
				// and return.
				return pgerror.WithConstraintName(errors.Wrapf(err, "failed to satisfy CHECK constraint (%s)", checks[i].Expr), checks[i].Name)
			}
			return pgerror.WithConstraintName(pgerror.Newf(
				pgcode.CheckViolation, "failed to satisfy CHECK constraint (%s)", expr,
			), checks[i].Name)
		}
		colIdx++
	}
	return nil
}
