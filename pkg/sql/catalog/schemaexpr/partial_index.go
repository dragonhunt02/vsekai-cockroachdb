// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package schemaexpr

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/transform"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// ValidatePartialIndexPredicate verifies that an expression is a valid partial
// index predicate. If the expression is valid, it returns the serialized
// expression with the columns dequalified.
//
// A predicate expression is valid if all of the following are true:
//
//   - It results in a boolean.
//   - It refers only to columns in the table.
//   - It does not include subqueries.
//   - It does not include non-immutable, aggregate, window, or set returning
//     functions.
//   - It does not reference a column which is in the process of being added
//     or removed.
//
func ValidatePartialIndexPredicate(
	ctx context.Context,
	desc catalog.MutableTableDescriptor,
	e tree.Expr,
	tn *tree.TableName,
	semaCtx *tree.SemaContext,
) (string, error) {
	expr, _, cols, err := DequalifyAndValidateExpr(
		ctx,
		desc,
		e,
		types.Bool,
		"index predicate",
		semaCtx,
		tree.VolatilityImmutable,
		tn,
	)
	if err != nil {
		return "", err
	}
	if !desc.IsNew() {
		if err := validatePartialIndexExprColsArePublic(desc, cols); err != nil {
			return "", err
		}
	}
	return expr, nil
}

func validatePartialIndexExprColsArePublic(
	desc catalog.TableDescriptor, cols catalog.TableColSet,
) (err error) {
	cols.ForEach(func(colID descpb.ColumnID) {
		if err != nil {
			return
		}
		var col catalog.Column
		col, err = desc.FindColumnWithID(colID)
		if err != nil {
			return
		}
		if col.Public() {
			return
		}
		err = pgerror.WithCandidateCode(errors.Errorf(
			"cannot create partial index on column %q (%d) which is not public",
			col.GetName(), col.GetID()),
			pgcode.FeatureNotSupported,
		)
	})
	return err
}

// MakePartialIndexExprs returns a map of predicate expressions for each
// partial index in the input list of indexes, or nil if none of the indexes
// are partial indexes. It also returns a set of all column IDs referenced in
// the expressions.
//
// If the expressions are being built within the context of an index add
// mutation in a transaction, the cols argument must include mutation columns
// that are added previously in the same transaction.
func MakePartialIndexExprs(
	ctx context.Context,
	indexes []catalog.Index,
	cols []catalog.Column,
	tableDesc catalog.TableDescriptor,
	evalCtx *tree.EvalContext,
	semaCtx *tree.SemaContext,
) (_ map[descpb.IndexID]tree.TypedExpr, refColIDs catalog.TableColSet, _ error) {
	// If none of the indexes are partial indexes, return early.
	partialIndexCount := 0
	for i := range indexes {
		if indexes[i].IsPartial() {
			partialIndexCount++
		}
	}
	if partialIndexCount == 0 {
		return nil, refColIDs, nil
	}

	exprs := make(map[descpb.IndexID]tree.TypedExpr, partialIndexCount)

	tn := tree.NewUnqualifiedTableName(tree.Name(tableDesc.GetName()))
	nr := newNameResolver(evalCtx, tableDesc.GetID(), tn, cols)
	nr.addIVarContainerToSemaCtx(semaCtx)

	var txCtx transform.ExprTransformContext
	for _, idx := range indexes {
		if idx.IsPartial() {
			expr, err := parser.ParseExpr(idx.GetPredicate())
			if err != nil {
				return nil, refColIDs, err
			}

			// Collect all column IDs that are referenced in the partial index
			// predicate expression.
			colIDs, err := ExtractColumnIDs(tableDesc, expr)
			if err != nil {
				return nil, refColIDs, err
			}
			refColIDs.UnionWith(colIDs)

			expr, err = nr.resolveNames(expr)
			if err != nil {
				return nil, refColIDs, err
			}

			typedExpr, err := tree.TypeCheck(ctx, expr, semaCtx, types.Bool)
			if err != nil {
				return nil, refColIDs, err
			}

			if typedExpr, err = txCtx.NormalizeExpr(evalCtx, typedExpr); err != nil {
				return nil, refColIDs, err
			}

			exprs[idx.GetID()] = typedExpr
		}
	}

	return exprs, refColIDs, nil
}
