// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package jobs

import (
	"context"
	gosql "database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobstest"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/stretchr/testify/require"
)

func TestScheduleControl(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	th, cleanup := newTestHelper(t)
	defer cleanup()

	t.Run("non-existent", func(t *testing.T) {
		for _, command := range []string{
			"PAUSE SCHEDULE 123",
			"PAUSE SCHEDULES SELECT 123",
			"RESUME SCHEDULE 123",
			"RESUME SCHEDULES SELECT schedule_id FROM system.scheduled_jobs",
			"DROP SCHEDULE 123",
			"DROP SCHEDULES SELECT schedule_id FROM system.scheduled_jobs",
		} {
			t.Run(command, func(t *testing.T) {
				th.sqlDB.ExecRowsAffected(t, 0, command)
			})
		}
	})

	ctx := context.Background()

	var recurringNever string

	makeSchedule := func(name string, cron string) int64 {
		schedule := th.newScheduledJob(t, name, "sql")
		if cron != "" {
			require.NoError(t, schedule.SetSchedule(cron))
		}
		require.NoError(t, schedule.Create(ctx, th.cfg.InternalExecutor, nil))
		return schedule.ScheduleID()
	}

	t.Run("pause-one-schedule", func(t *testing.T) {
		scheduleID := makeSchedule("one-schedule", "@daily")
		th.sqlDB.Exec(t, "PAUSE SCHEDULE $1", scheduleID)
		require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
	})

	t.Run("pause-one-off-schedule", func(t *testing.T) {
		scheduleID := makeSchedule("one-schedule", recurringNever)
		th.sqlDB.Exec(t, "PAUSE SCHEDULE $1", scheduleID)
		require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
	})

	t.Run("pause-active-schedule", func(t *testing.T) {
		schedule := th.newScheduledJob(t, "test schedule", "select 42")
		require.NoError(t, schedule.SetSchedule("@weekly"))
		// Datums only store up until microseconds.
		ms := time.Microsecond
		firstRunTime := timeutil.Now().Add(10 * time.Second).Truncate(ms)
		schedule.SetNextRun(firstRunTime)
		require.NoError(t, schedule.Create(ctx, th.cfg.InternalExecutor, nil))
		scheduleID := schedule.ScheduleID()
		require.Equal(t, schedule.NextRun(), firstRunTime)
		th.sqlDB.Exec(t, "RESUME SCHEDULE $1", scheduleID)

		afterSchedule := th.loadSchedule(t, scheduleID)
		require.False(t, afterSchedule.IsPaused())
		require.Equal(t, afterSchedule.NextRun(), firstRunTime)
	})

	t.Run("cannot-resume-one-off-schedule", func(t *testing.T) {
		schedule := th.newScheduledJob(t, "test schedule", "select 42")
		require.NoError(t, schedule.Create(ctx, th.cfg.InternalExecutor, nil))

		th.sqlDB.ExpectErr(t, "cannot set next run for schedule",
			"RESUME SCHEDULE $1", schedule.ScheduleID())
	})

	t.Run("pause-and-resume-one-schedule", func(t *testing.T) {
		scheduleID := makeSchedule("one-schedule", "@daily")
		th.sqlDB.Exec(t, "PAUSE SCHEDULE $1", scheduleID)
		require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
		th.sqlDB.Exec(t, "RESUME SCHEDULE $1", scheduleID)

		schedule := th.loadSchedule(t, scheduleID)
		require.False(t, schedule.IsPaused())
	})

	t.Run("pause-resume-and-drop-many-schedules", func(t *testing.T) {
		var scheduleIDs []int64
		for i := 0; i < 10; i++ {
			scheduleIDs = append(
				scheduleIDs,
				makeSchedule(fmt.Sprintf("pause-resume-many-%d", i), "@daily"),
			)
		}

		querySchedules := "SELECT schedule_id FROM " + th.env.ScheduledJobsTableName() +
			" WHERE schedule_name LIKE 'pause-resume-many-%'"

		th.sqlDB.Exec(t, "PAUSE SCHEDULES "+querySchedules)

		for _, scheduleID := range scheduleIDs {
			require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
		}

		th.sqlDB.Exec(t, "RESUME SCHEDULES "+querySchedules)

		for _, scheduleID := range scheduleIDs {
			require.False(t, th.loadSchedule(t, scheduleID).IsPaused())
		}

		th.sqlDB.Exec(t, "DROP SCHEDULES "+querySchedules)
		require.Equal(t, 0, len(th.sqlDB.QueryStr(t, querySchedules)))
	})

	t.Run("pause-non-privileged-user", func(t *testing.T) {
		scheduleID := makeSchedule("one-schedule", "@daily")

		th.sqlDB.Exec(t, `CREATE USER testuser`)
		pgURL, cleanupFunc := sqlutils.PGUrl(
			t, th.server.ServingSQLAddr(), "NonPrivileged-testuser",
			url.User("testuser"),
		)
		defer cleanupFunc()
		testuser, err := gosql.Open("postgres", pgURL.String())
		require.NoError(t, err)
		defer testuser.Close()

		_, err = testuser.Exec("PAUSE SCHEDULE $1", scheduleID)
		require.EqualError(t, err, "pq: only users with the admin role are allowed to PAUSE SCHEDULES")
	})
}

func TestJobsControlForSchedules(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	th, cleanup := newTestHelperForTables(t, jobstest.UseSystemTables, nil)
	defer cleanup()

	registry := th.server.JobRegistry().(*Registry)
	blockResume := make(chan struct{})
	defer close(blockResume)

	// Our resume never completes any jobs, until this test completes.
	// As such, the job does not undergo usual job state transitions
	// (e.g. pause-request -> paused).
	RegisterConstructor(jobspb.TypeImport, func(job *Job, _ *cluster.Settings) Resumer {
		return FakeResumer{
			OnResume: func(_ context.Context) error {
				<-blockResume
				return nil
			},
		}
	}, UsesTenantCostControl)

	record := Record{
		Description: "fake job",
		Username:    security.TestUserName(),
		Details:     jobspb.ImportDetails{},
		Progress:    jobspb.ImportProgress{},
	}

	const numJobs = 5

	// Create few jobs not started by any schedule.
	for i := 0; i < numJobs; i++ {
		_, err := registry.CreateAdoptableJobWithTxn(
			context.Background(), record, registry.MakeJobID(), nil, /* txn */
		)
		require.NoError(t, err)
	}

	var scheduleID int64 = 123

	for _, tc := range []struct {
		command      string
		numSchedules int
	}{
		{"pause", 1},
		{"resume", 1},
		{"cancel", 1},
		{"pause", 2},
		{"resume", 3},
		{"cancel", 4},
	} {
		schedulesStr := &strings.Builder{}
		for i := 0; i < tc.numSchedules; i++ {
			scheduleID++
			if schedulesStr.Len() > 0 {
				schedulesStr.WriteByte(',')
			}
			fmt.Fprintf(schedulesStr, "%d", scheduleID)

			for i := 0; i < numJobs; i++ {
				record.CreatedBy = &CreatedByInfo{
					Name: CreatedByScheduledJobs,
					ID:   scheduleID,
				}
				jobID := registry.MakeJobID()
				_, err := registry.CreateAdoptableJobWithTxn(
					context.Background(), record, jobID, nil, /* txn */
				)
				require.NoError(t, err)

				if tc.command == "resume" {
					// Job has to be in paused state in order for it to be resumable;
					// Alas, because we don't actually run real jobs (see comment above),
					// We can't just pause the job (since it will stay in pause-requested state forever).
					// So, just force set job status to paused.
					th.sqlDB.Exec(t, "UPDATE system.jobs SET status=$1 WHERE id=$2", StatusPaused,
						jobID)
				}
			}
		}

		jobControl := tc.command + " JOBS FOR "
		if tc.numSchedules == 1 {
			jobControl += "SCHEDULE " + schedulesStr.String()
		} else {
			jobControl += fmt.Sprintf("SCHEDULES SELECT unnest(array[%s])", schedulesStr)
		}

		t.Run(jobControl, func(t *testing.T) {
			// Go through internal executor to execute job control command.
			// This correctly reports the number of effected rows.
			numEffected, err := th.cfg.InternalExecutor.ExecEx(
				context.Background(),
				"test-num-effected",
				nil,
				sessiondata.InternalExecutorOverride{User: security.RootUserName()},
				jobControl,
			)
			require.NoError(t, err)
			require.Equal(t, numJobs*tc.numSchedules, numEffected)
		})
	}
}

// TestFilterJobsControlForSchedules tests that a ControlJobsForSchedules query
// does not error out even if the schedule contains a job in a state which is
// invalid to apply the control command to. This is done by filtering out such
// jobs prior to executing the control command.
func TestFilterJobsControlForSchedules(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer ResetConstructors()()

	argsFn := func(args *base.TestServerArgs) {
		// Prevent registry from changing job state while running this test.
		interval := 24 * time.Hour
		args.Knobs.JobsTestingKnobs = NewTestingKnobsWithIntervals(interval, interval, interval, interval)
	}
	th, cleanup := newTestHelperForTables(t, jobstest.UseSystemTables, argsFn)
	defer cleanup()

	registry := th.server.JobRegistry().(*Registry)
	blockResume := make(chan struct{})
	defer close(blockResume)

	// Our resume never completes any jobs, until this test completes.
	RegisterConstructor(jobspb.TypeImport, func(job *Job, _ *cluster.Settings) Resumer {
		return FakeResumer{
			OnResume: func(_ context.Context) error {
				<-blockResume
				return nil
			},
		}
	}, UsesTenantCostControl)

	record := Record{
		Description: "fake job",
		Username:    security.TestUserName(),
		Details:     jobspb.ImportDetails{},
		Progress:    jobspb.ImportProgress{},
	}

	allJobStates := []Status{StatusPending, StatusRunning, StatusPaused, StatusFailed,
		StatusReverting, StatusSucceeded, StatusCanceled, StatusCancelRequested, StatusPauseRequested}

	var scheduleID int64 = 123
	for _, tc := range []struct {
		command             string
		validStartingStates []Status
	}{
		{"pause", []Status{StatusPending, StatusRunning, StatusReverting}},
		{"resume", []Status{StatusPaused}},
		{"cancel", []Status{StatusPending, StatusRunning, StatusPaused}},
	} {
		scheduleID++
		// Create one job of every Status.
		for _, status := range allJobStates {
			record.CreatedBy = &CreatedByInfo{
				Name: CreatedByScheduledJobs,
				ID:   scheduleID,
			}
			jobID := registry.MakeJobID()
			_, err := registry.CreateAdoptableJobWithTxn(context.Background(), record, jobID, nil /* txn */)
			require.NoError(t, err)
			th.sqlDB.Exec(t, "UPDATE system.jobs SET status=$1 WHERE id=$2", status, jobID)
		}

		jobControl := fmt.Sprintf(tc.command+" JOBS FOR SCHEDULE %d", scheduleID)
		t.Run(jobControl, func(t *testing.T) {
			// Go through internal executor to execute job control command. This
			// correctly reports the number of effected rows which should only be
			// equal to the number of validStartingStates as all the other states are
			// invalid/no-ops.
			numEffected, err := th.cfg.InternalExecutor.ExecEx(
				context.Background(),
				"test-num-effected",
				nil,
				sessiondata.InternalExecutorOverride{User: security.RootUserName()},
				jobControl,
			)
			require.NoError(t, err)
			require.Equal(t, len(tc.validStartingStates), numEffected)
		})

		// Clear the system.jobs table for the next test run.
		th.sqlDB.Exec(t, "DELETE FROM system.jobs")
	}
}

func TestJobControlByType(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer ResetConstructors()()

	argsFn := func(args *base.TestServerArgs) {
		// Prevent registry from changing job state while running this test.
		interval := 24 * time.Hour
		args.Knobs.JobsTestingKnobs = NewTestingKnobsWithIntervals(interval, interval, interval, interval)
	}
	th, cleanup := newTestHelperForTables(t, jobstest.UseSystemTables, argsFn)
	defer cleanup()

	registry := th.server.JobRegistry().(*Registry)
	blockResume := make(chan struct{})
	defer close(blockResume)

	t.Run("Errors if invalid type is specified", func(t *testing.T) {
		invalidTypeQuery := "PAUSE ALL blah JOBS"
		_, err := th.cfg.InternalExecutor.ExecEx(
			context.Background(),
			"test-invalid-type",
			nil,
			sessiondata.InternalExecutorOverride{User: security.RootUserName()},
			invalidTypeQuery,
		)
		require.Error(t, err)
	})

	// To test the commands on valid job types, one job of every type in every state will be created
	var allJobTypes = []jobspb.Type{jobspb.TypeChangefeed, jobspb.TypeImport, jobspb.TypeBackup, jobspb.TypeRestore}
	var jobspbTypeToString = map[jobspb.Type]string{
		jobspb.TypeChangefeed: "CHANGEFEED",
		jobspb.TypeBackup:     "BACKUP",
		jobspb.TypeImport:     "IMPORT",
		jobspb.TypeRestore:    "RESTORE",
	}

	var allJobStates = []Status{StatusPending, StatusRunning, StatusPaused, StatusFailed,
		StatusReverting, StatusSucceeded, StatusCanceled, StatusCancelRequested, StatusPauseRequested}

	// This is required to make the jobs of each type controllable
	for _, jobType := range allJobTypes {
		RegisterConstructor(jobType, func(job *Job, _ *cluster.Settings) Resumer {
			return FakeResumer{
				OnResume: func(ctx context.Context) error {
					<-ctx.Done()
					return nil
				},
			}
		}, UsesTenantCostControl)
	}

	for _, jobType := range allJobTypes {
		for _, tc := range []struct {
			command        string
			startingStates []Status
			endState       Status
		}{
			{"pause", []Status{StatusPending, StatusRunning, StatusReverting}, StatusPauseRequested},
			{"resume", []Status{StatusPaused}, StatusRunning},
			{"cancel", []Status{StatusPending, StatusRunning, StatusPaused}, StatusCancelRequested},
		} {
			commandQuery := fmt.Sprintf("%s ALL %s JOBS", tc.command, jobspbTypeToString[jobType])
			t.Run(commandQuery, func(t *testing.T) {
				var jobIDStrings []string

				// Make multiple jobs of every permutation of job type and job state
				const numJobsPerStatus = 3
				for _, jobInfo := range []struct {
					jobDetails  jobspb.Details
					jobProgress jobspb.ProgressDetails
				}{
					{jobspb.ChangefeedDetails{}, jobspb.ChangefeedProgress{}},
					{jobspb.ImportDetails{}, jobspb.ImportProgress{}},
					{jobspb.BackupDetails{}, jobspb.BackupProgress{}},
					{jobspb.RestoreDetails{}, jobspb.RestoreProgress{}},
				} {
					for _, status := range allJobStates {
						for i := 0; i < numJobsPerStatus; i++ {
							record := Record{
								Description: "fake job",
								Username:    security.TestUserName(),
								Details:     jobInfo.jobDetails,
								Progress:    jobInfo.jobProgress,
							}

							jobID := registry.MakeJobID()
							jobIDStrings = append(jobIDStrings, fmt.Sprintf("%d", jobID))
							_, err := registry.CreateAdoptableJobWithTxn(context.Background(), record, jobID, nil /* txn */)
							require.NoError(t, err)
							th.sqlDB.Exec(t, "UPDATE system.jobs SET status=$1 WHERE id=$2", status, jobID)
						}
					}
				}

				jobIdsClause := fmt.Sprint(strings.Join(jobIDStrings, ", "))

				// Execute the command and verify its executed on the expected number of rows
				numEffected, err := th.cfg.InternalExecutor.ExecEx(
					context.Background(),
					"test-num-effected",
					nil,
					sessiondata.InternalExecutorOverride{User: security.RootUserName()},
					commandQuery,
				)
				require.NoError(t, err)

				// Jobs in the starting state should be affected
				numExpectedJobsAffected := numJobsPerStatus * len(tc.startingStates)
				require.Equal(t, numExpectedJobsAffected, numEffected)

				// Both the affected jobs + the jobs originally in the target state should be in that state
				numExpectedJobsWithEndState := numExpectedJobsAffected + numJobsPerStatus

				// By verifying that the correct number of jobs are in the expected end state and
				// the expected number of jobs were affected by the command, we guarantee that
				// only the expected jobs have changed
				var numJobs = 0
				th.sqlDB.QueryRow(
					t,
					fmt.Sprintf(
						"SELECT count(*) FROM [SHOW JOBS] WHERE status='%s' AND job_type='%s' AND job_id IN (%s)",
						tc.endState, jobType, jobIdsClause,
					),
				).Scan(&numJobs)
				require.Equal(t, numJobs, numExpectedJobsWithEndState)

				// Clear the system.jobs table for the next test run.
				th.sqlDB.Exec(t, fmt.Sprintf("DELETE FROM system.jobs WHERE id IN (%s)", jobIdsClause))
			})
		}
	}
}
