// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql_test

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/randgen"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/tests"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeofday"
	"github.com/cockroachdb/cockroach/pkg/util/timetz"
	"github.com/jackc/pgx/v4"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyNullInfNaN(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	params, _ := tests.CreateTestServerParams()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (
			i INT NULL,
			f FLOAT NULL,
			s STRING NULL,
			b BYTES NULL,
			d DATE NULL,
			t TIME NULL,
			ttz TIME NULL,
			ts TIMESTAMP NULL,
			n INTERVAL NULL,
			o BOOL NULL,
			e DECIMAL NULL,
			u UUID NULL,
			ip INET NULL,
			tz TIMESTAMPTZ NULL,
			geography GEOGRAPHY NULL,
			geometry GEOMETRY NULL,
			box2d BOX2D NULL
		);
	`); err != nil {
		t.Fatal(err)
	}

	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	stmt, err := txn.Prepare(pq.CopyIn(
		"t", "i", "f", "s", "b", "d", "t", "ttz",
		"ts", "n", "o", "e", "u", "ip", "tz", "geography", "geometry", "box2d"))
	if err != nil {
		t.Fatal(err)
	}

	input := [][]interface{}{
		{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		{nil, math.Inf(1), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		{nil, math.Inf(-1), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		{nil, math.NaN(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
	}

	for _, in := range input {
		_, err = stmt.Exec(in...)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = stmt.Exec()
	if err != nil {
		t.Fatal(err)
	}
	err = stmt.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = txn.Commit()
	if err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query("SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for row, in := range input {
		if !rows.Next() {
			t.Fatal("expected more results")
		}
		data := make([]interface{}, len(in))
		for i := range data {
			data[i] = new(interface{})
		}
		if err := rows.Scan(data...); err != nil {
			t.Fatal(err)
		}
		for i, d := range data {
			v := d.(*interface{})
			d = *v
			if a, b := fmt.Sprintf("%#v", d), fmt.Sprintf("%#v", in[i]); a != b {
				t.Fatalf("row %v, col %v: got %#v (%T), expected %#v (%T)", row, i, d, d, in[i], in[i])
			}
		}
	}
}

// TestCopyRandom inserts random rows using COPY and ensures the SELECT'd
// data is the same.
func TestCopyRandom(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	params, _ := tests.CreateTestServerParams()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	if _, err := db.Exec(`
		CREATE DATABASE d;
		CREATE TABLE IF NOT EXISTS d.t (
			id INT PRIMARY KEY,
			n INTERVAL,
			cs TEXT COLLATE en_us_u_ks_level2,
			o BOOL,
			i INT,
			f FLOAT,
			e DECIMAL,
			t TIME,
			ttz TIMETZ,
			ts TIMESTAMP,
			s STRING,
			b BYTES,
			u UUID,
			ip INET,
			tz TIMESTAMPTZ,
			geography GEOGRAPHY NULL,
			geometry GEOMETRY NULL,
			box2d BOX2D NULL
		);
		SET extra_float_digits = 3; -- to preserve floats entirely
	`); err != nil {
		t.Fatal(err)
	}

	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	stmt, err := txn.Prepare(pq.CopyInSchema("d", "t", "id", "n", "cs", "o", "i", "f", "e", "t", "ttz", "ts", "s", "b", "u", "ip", "tz", "geography", "geometry", "box2d"))
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(0))
	typs := []*types.T{
		types.Int,
		types.Interval,
		types.MakeCollatedString(types.String, "en_us_u_ks_level2"),
		types.Bool,
		types.Int,
		types.Float,
		types.Decimal,
		types.Time,
		types.TimeTZ,
		types.Timestamp,
		types.String,
		types.Bytes,
		types.Uuid,
		types.INet,
		types.TimestampTZ,
		types.Geography,
		types.Geometry,
		types.Box2D,
	}

	var inputs [][]interface{}

	for i := 0; i < 1000; i++ {
		row := make([]interface{}, len(typs))
		for j, t := range typs {
			var ds string
			if j == 0 {
				// Special handling for ID field
				ds = strconv.Itoa(i)
			} else {
				d := randgen.RandDatum(rng, t, false)
				ds = tree.AsStringWithFlags(d, tree.FmtBareStrings)
				switch t.Family() {
				case types.CollatedStringFamily:
					// For collated strings, we just want the raw contents in COPY.
					ds = d.(*tree.DCollatedString).Contents
				case types.FloatFamily:
					ds = strings.TrimSuffix(ds, ".0")
				}
			}
			row[j] = ds
		}
		_, err = stmt.Exec(row...)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, row)
	}

	err = stmt.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = txn.Commit()
	if err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query("SELECT * FROM d.t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for row, in := range inputs {
		if !rows.Next() {
			t.Fatal("expected more results")
		}
		data := make([]interface{}, len(in))
		for i := range data {
			data[i] = new(interface{})
		}
		if err := rows.Scan(data...); err != nil {
			t.Fatal(err)
		}
		for i, d := range data {
			v := d.(*interface{})
			d := *v
			ds := fmt.Sprint(d)
			switch d := d.(type) {
			case []byte:
				ds = string(d)
			case time.Time:
				var dt tree.NodeFormatter
				if typs[i].Family() == types.TimeFamily {
					dt = tree.MakeDTime(timeofday.FromTimeAllow2400(d))
				} else if typs[i].Family() == types.TimeTZFamily {
					dt = tree.NewDTimeTZ(timetz.MakeTimeTZFromTimeAllow2400(d))
				} else if typs[i].Family() == types.TimestampFamily {
					dt = tree.MustMakeDTimestamp(d, time.Microsecond)
				} else {
					dt = tree.MustMakeDTimestampTZ(d, time.Microsecond)
				}
				ds = tree.AsStringWithFlags(dt, tree.FmtBareStrings)
			}
			if !reflect.DeepEqual(in[i], ds) {
				t.Fatalf("row %v, col %v: got %#v (%T), expected %#v", row, i, ds, d, in[i])
			}
		}
	}
}

// TestCopyBinary uses the pgx driver, which hard codes COPY ... BINARY.
func TestCopyBinary(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	params, _ := tests.CreateTestServerParams()
	s, db, _ := serverutils.StartServer(t, params)
	sqlDB := sqlutils.MakeSQLRunner(db)
	defer s.Stopper().Stop(ctx)

	pgURL, cleanupGoDB := sqlutils.PGUrl(
		t, s.ServingSQLAddr(), "StartServer" /* prefix */, url.User(security.RootUser))
	defer cleanupGoDB()
	conn, err := pgx.Connect(ctx, pgURL.String())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Exec(ctx, `
		CREATE TABLE t (
			id INT8 PRIMARY KEY,
			u INT, -- NULL test
			o BOOL,
			i2 INT2,
			i4 INT4,
			i8 INT8,
			f FLOAT,
			s STRING,
			b BYTES
		);
	`); err != nil {
		t.Fatal(err)
	}

	input := [][]interface{}{{
		1,
		nil,
		true,
		int16(1),
		int32(1),
		int64(1),
		float64(1),
		"s",
		"b",
	}}
	if _, err = conn.CopyFrom(
		ctx,
		pgx.Identifier{"t"},
		[]string{"id", "u", "o", "i2", "i4", "i8", "f", "s", "b"},
		pgx.CopyFromRows(input),
	); err != nil {
		t.Fatal(err)
	}

	expect := func() [][]string {
		row := make([]string, len(input[0]))
		for i, v := range input[0] {
			if v == nil {
				row[i] = "NULL"
			} else {
				row[i] = fmt.Sprintf("%v", v)
			}
		}
		return [][]string{row}
	}()
	sqlDB.CheckQueryResults(t, "SELECT * FROM t ORDER BY id", expect)
}

func TestCopyError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	params, _ := tests.CreateTestServerParams()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (
			i INT PRIMARY KEY
		);
	`); err != nil {
		t.Fatal(err)
	}

	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	stmt, err := txn.Prepare(pq.CopyIn("t", "i"))
	if err != nil {
		t.Fatal(err)
	}

	// Insert conflicting primary keys.
	for i := 0; i < 2; i++ {
		_, err = stmt.Exec(1)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = stmt.Close()
	if err == nil {
		t.Fatal("expected error")
	}

	// Make sure we can query after an error.
	var i int
	if err := db.QueryRow("SELECT 1").Scan(&i); err != nil {
		t.Fatal(err)
	} else if i != 1 {
		t.Fatalf("expected 1, got %d", i)
	}
	if err := txn.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// TestCopyTrace verifies copy works with tracing turned on.
func TestCopyTrace(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	for _, strs := range [][]string{
		{`SET CLUSTER SETTING sql.trace.log_statement_execute = true`},
		{`SET CLUSTER SETTING sql.telemetry.query_sampling.enabled = true`},
		{`SET CLUSTER SETTING sql.log.unstructured_entries.enabled = true`, `SET CLUSTER SETTING sql.trace.log_statement_execute = true`},
		{`SET CLUSTER SETTING sql.log.admin_audit.enabled = true`},
	} {
		t.Run(strs[0], func(t *testing.T) {
			params, _ := tests.CreateTestServerParams()
			s, db, _ := serverutils.StartServer(t, params)
			defer s.Stopper().Stop(context.Background())

			_, err := db.Exec(`
		CREATE TABLE t (
			i INT PRIMARY KEY
		);
	`)
			require.NoError(t, err)

			for _, str := range strs {
				_, err = db.Exec(str)
				require.NoError(t, err)
			}

			// We have to start a new connection every time to exercise all possible paths.
			t.Run("success", func(t *testing.T) {
				db := serverutils.OpenDBConn(
					t, s.ServingSQLAddr(), params.UseDatabase, params.Insecure, s.Stopper())
				require.NoError(t, err)
				txn, err := db.Begin()
				const val = 2
				require.NoError(t, err)
				{
					stmt, err := txn.Prepare(pq.CopyIn("t", "i"))
					require.NoError(t, err)
					_, err = stmt.Exec(val)
					require.NoError(t, err)
					require.NoError(t, stmt.Close())
				}
				require.NoError(t, txn.Commit())

				var i int
				require.NoError(t, db.QueryRow("SELECT i FROM t").Scan(&i))
				require.Equal(t, val, i)
			})

			t.Run("error in statement", func(t *testing.T) {
				db := serverutils.OpenDBConn(
					t, s.ServingSQLAddr(), params.UseDatabase, params.Insecure, s.Stopper())
				txn, err := db.Begin()
				require.NoError(t, err)
				{
					_, err := txn.Prepare(pq.CopyIn("xxx", "yyy"))
					require.Error(t, err)
					require.True(t, strings.Contains(err.Error(), `relation "xxx" does not exist`))
				}
				require.NoError(t, txn.Rollback())
			})

			t.Run("error during copy", func(t *testing.T) {
				db := serverutils.OpenDBConn(
					t, s.ServingSQLAddr(), params.UseDatabase, params.Insecure, s.Stopper())
				txn, err := db.Begin()
				require.NoError(t, err)
				{
					stmt, err := txn.Prepare(pq.CopyIn("t", "i"))
					require.NoError(t, err)
					_, err = stmt.Exec("bob")
					require.NoError(t, err)
					err = stmt.Close()
					require.Error(t, err)
					require.True(t, strings.Contains(err.Error(), `could not parse "bob" as type int`))
				}
				require.NoError(t, txn.Rollback())
			})

			t.Run("error during insert phase of copy", func(t *testing.T) {
				db := serverutils.OpenDBConn(
					t, s.ServingSQLAddr(), params.UseDatabase, params.Insecure, s.Stopper())
				txn, err := db.Begin()
				require.NoError(t, err)
				{
					stmt, err := txn.Prepare(pq.CopyIn("t", "i"))
					require.NoError(t, err)
					_, err = stmt.Exec("2")
					require.NoError(t, err)
					err = stmt.Close()
					require.Error(t, err)
					require.True(t, strings.Contains(err.Error(), `duplicate key value violates unique constraint "t_pkey"`))
				}
				require.NoError(t, txn.Rollback())
			})
		})
	}
}

// TestCopyFromRetries tests copy from works as expected for certain retry
// states.
func TestCopyFromRetries(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const numRows = 100

	testCases := []struct {
		desc           string
		hook           func(attemptNum int) error
		retriesEnabled bool
		inTxn          bool
		expectedRows   int
		expectedErr    bool
	}{
		{
			desc:           "does not attempt to retry if disabled",
			retriesEnabled: false,
			hook: func(attemptNum int) error {
				if attemptNum == 1 {
					return &roachpb.TransactionRetryWithProtoRefreshError{}
				}
				return nil
			},
			expectedErr: true,
		},
		{
			desc:           "fails to retry in txn",
			retriesEnabled: true,
			inTxn:          true,
			hook: func(attemptNum int) error {
				if attemptNum == 1 {
					return &roachpb.TransactionRetryWithProtoRefreshError{}
				}
				return nil
			},
			expectedErr: true,
		},
		{
			desc:           "retries successfully on every batch",
			retriesEnabled: true,
			hook: func(attemptNum int) error {
				if attemptNum%2 == 1 {
					return &roachpb.TransactionRetryWithProtoRefreshError{}
				}
				return nil
			},
			expectedRows: numRows,
		},
		{
			desc:           "eventually dies on too many restarts",
			retriesEnabled: true,
			hook: func(attemptNum int) error {
				return &roachpb.TransactionRetryWithProtoRefreshError{}
			},
			expectedErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			params, _ := tests.CreateTestServerParams()
			var attemptNumber int
			params.Knobs.SQLExecutor = &sql.ExecutorTestingKnobs{
				BeforeCopyFromInsert: func() error {
					attemptNumber++
					return tc.hook(attemptNumber)
				},
			}
			s, db, _ := serverutils.StartServer(t, params)
			defer s.Stopper().Stop(context.Background())

			_, err := db.Exec(
				`CREATE TABLE t (
					i INT PRIMARY KEY
				);`,
			)
			require.NoError(t, err)

			ctx := context.Background()

			// Use pgx instead of lib/pq as pgx doesn't require copy to be in a txn.
			pgURL, cleanupGoDB := sqlutils.PGUrl(
				t, s.ServingSQLAddr(), "StartServer" /* prefix */, url.User(security.RootUser))
			defer cleanupGoDB()
			pgxConn, err := pgx.Connect(ctx, pgURL.String())
			require.NoError(t, err)
			_, err = pgxConn.Exec(ctx, "SET copy_from_retries_enabled = $1", fmt.Sprintf("%t", tc.retriesEnabled))
			require.NoError(t, err)

			if err := func() error {
				var rows [][]interface{}
				for i := 0; i < numRows; i++ {
					rows = append(rows, []interface{}{i})
				}
				if !tc.inTxn {
					_, err := pgxConn.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"i"}, pgx.CopyFromRows(rows))
					return err
				}

				txn, err := pgxConn.Begin(ctx)
				if err != nil {
					return err
				}
				defer func() {
					_ = txn.Rollback(ctx)
				}()
				_, err = txn.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"i"}, pgx.CopyFromRows(rows))
				if err != nil {
					return err
				}
				return txn.Commit(ctx)
			}(); err != nil {
				assert.True(t, tc.expectedErr, "got error %+v", err)
			} else {
				assert.False(t, tc.expectedErr, "expected error but got none")
			}

			var actualRows int
			err = db.QueryRow("SELECT count(1) FROM t").Scan(&actualRows)
			require.NoError(t, err)
			require.Equal(t, tc.expectedRows, actualRows)
		})
	}
}

// TestCopyTransaction verifies that COPY data can be used after it is done
// within a transaction.
func TestCopyTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	params, _ := tests.CreateTestServerParams()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	if _, err := db.Exec(`
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE t (
			i INT PRIMARY KEY
		);
	`); err != nil {
		t.Fatal(err)
	}

	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	// Note that, at least with lib/pq, this doesn't actually send a Parse msg
	// (which we wouldn't support, as we don't support Copy-in in extended
	// protocol mode). lib/pq has magic for recognizing a Copy.
	stmt, err := txn.Prepare(pq.CopyIn("t", "i"))
	if err != nil {
		t.Fatal(err)
	}

	const val = 2

	_, err = stmt.Exec(val)
	if err != nil {
		t.Fatal(err)
	}

	if err = stmt.Close(); err != nil {
		t.Fatal(err)
	}

	var i int
	if err := txn.QueryRow("SELECT i FROM d.t").Scan(&i); err != nil {
		t.Fatal(err)
	} else if i != val {
		t.Fatalf("expected 1, got %d", i)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestCopyFKCheck verifies that foreign keys are checked during COPY.
func TestCopyFKCheck(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	params, _ := tests.CreateTestServerParams()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	db.SetMaxOpenConns(1)
	r := sqlutils.MakeSQLRunner(db)
	r.Exec(t, `
		CREATE DATABASE d;
		SET DATABASE = d;
		CREATE TABLE p (p INT PRIMARY KEY);
		CREATE TABLE t (
		  a INT PRIMARY KEY,
		  p INT REFERENCES p(p)
		);
	`)

	txn, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txn.Rollback() }()

	stmt, err := txn.Prepare(pq.CopyIn("t", "a", "p"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = stmt.Exec(1, 1)
	if err != nil {
		t.Fatal(err)
	}

	err = stmt.Close()
	if !testutils.IsError(err, "foreign key violation|violates foreign key constraint") {
		t.Fatalf("expected FK error, got: %v", err)
	}
}

// TestCopyInReleasesLeases is a regression test to ensure that the execution
// of CopyIn does not retain table descriptor leases after completing by
// attempting to run a schema change after performing a copy.
func TestCopyInReleasesLeases(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	params, _ := tests.CreateTestServerParams()
	s, db, _ := serverutils.StartServer(t, params)
	tdb := sqlutils.MakeSQLRunner(db)
	defer s.Stopper().Stop(ctx)
	tdb.Exec(t, `CREATE TABLE t (k INT8 PRIMARY KEY)`)
	tdb.Exec(t, `CREATE USER foo WITH PASSWORD 'testabc'`)
	tdb.Exec(t, `GRANT admin TO foo`)

	userURL, cleanupFn := sqlutils.PGUrlWithOptionalClientCerts(t,
		s.ServingSQLAddr(), t.Name(), url.UserPassword("foo", "testabc"),
		false /* withClientCerts */)
	defer cleanupFn()
	conn, err := pgxConn(t, userURL)
	require.NoError(t, err)

	rowsAffected, err := conn.CopyFrom(
		ctx,
		pgx.Identifier{"t"},
		[]string{"k"},
		pgx.CopyFromRows([][]interface{}{{1}, {2}}),
	)
	require.NoError(t, err)
	require.Equal(t, int64(2), rowsAffected)

	// Prior to the bug fix which prompted this test, the below schema change
	// would hang until the leases expire. Let's make sure it finishes "soon".
	alterErr := make(chan error, 1)
	go func() {
		_, err := db.Exec(`ALTER TABLE t ADD COLUMN v INT NOT NULL DEFAULT 0`)
		alterErr <- err
	}()
	select {
	case err := <-alterErr:
		require.NoError(t, err)
	case <-time.After(testutils.DefaultSucceedsSoonDuration):
		t.Fatal("alter did not complete")
	}
	require.NoError(t, conn.Close(ctx))
}
