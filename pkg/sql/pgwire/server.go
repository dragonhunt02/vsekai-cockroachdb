// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package pgwire

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkeys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/hba"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/identmap"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgwirebase"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgwirecancel"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/util/contextutil"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/cockroachdb/redact"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ATTENTION: After changing this value in a unit test, you probably want to
// open a new connection pool since the connections in the existing one are not
// affected.
//
// The "results_buffer_size" connection parameter can be used to override this
// default for an individual connection.
var connResultsBufferSize = settings.RegisterByteSizeSetting(
	settings.TenantWritable,
	"sql.defaults.results_buffer.size",
	"default size of the buffer that accumulates results for a statement or a batch "+
		"of statements before they are sent to the client. This can be overridden on "+
		"an individual connection with the 'results_buffer_size' parameter. Note that auto-retries "+
		"generally only happen while no results have been delivered to the client, so "+
		"reducing this size can increase the number of retriable errors a client "+
		"receives. On the other hand, increasing the buffer size can increase the "+
		"delay until the client receives the first result row. "+
		"Updating the setting only affects new connections. "+
		"Setting to 0 disables any buffering.",
	16<<10, // 16 KiB
).WithPublic()

var logConnAuth = settings.RegisterBoolSetting(
	settings.TenantWritable,
	sql.ConnAuditingClusterSettingName,
	"if set, log SQL client connect and disconnect events (note: may hinder performance on loaded nodes)",
	false).WithPublic()

var logSessionAuth = settings.RegisterBoolSetting(
	settings.TenantWritable,
	sql.AuthAuditingClusterSettingName,
	"if set, log SQL session login/disconnection events (note: may hinder performance on loaded nodes)",
	false).WithPublic()

var maxNumConnections = settings.RegisterIntSetting(
	settings.TenantWritable,
	"server.max_connections_per_gateway",
	"the maximum number of non-superuser SQL connections per gateway allowed at a given time "+
		"(note: this will only limit future connection attempts and will not affect already established connections). "+
		"Negative values result in unlimited number of connections. Superusers are not affected by this limit.",
	-1, // Postgres defaults to 100, but we default to -1 to match our previous behavior of unlimited.
).WithPublic()

const (
	// ErrSSLRequired is returned when a client attempts to connect to a
	// secure server in cleartext.
	ErrSSLRequired = "node is running secure mode, SSL connection required"

	// ErrDrainingNewConn is returned when a client attempts to connect to a server
	// which is not accepting client connections.
	ErrDrainingNewConn = "server is not accepting clients, try another node"
	// ErrDrainingExistingConn is returned when a connection is shut down because
	// the server is draining.
	ErrDrainingExistingConn = "server is shutting down"
)

// Fully-qualified names for metrics.
var (
	MetaConns = metric.Metadata{
		Name:        "sql.conns",
		Help:        "Number of open SQL connections",
		Measurement: "Connections",
		Unit:        metric.Unit_COUNT,
	}
	MetaNewConns = metric.Metadata{
		Name:        "sql.new_conns",
		Help:        "Counter of the number of sql connections created",
		Measurement: "Connections",
		Unit:        metric.Unit_COUNT,
	}
	MetaBytesIn = metric.Metadata{
		Name:        "sql.bytesin",
		Help:        "Number of sql bytes received",
		Measurement: "SQL Bytes",
		Unit:        metric.Unit_BYTES,
	}
	MetaBytesOut = metric.Metadata{
		Name:        "sql.bytesout",
		Help:        "Number of sql bytes sent",
		Measurement: "SQL Bytes",
		Unit:        metric.Unit_BYTES,
	}
	MetaConnLatency = metric.Metadata{
		Name:        "sql.conn.latency",
		Help:        "Latency to establish and authenticate a SQL connection",
		Measurement: "Nanoseconds",
		Unit:        metric.Unit_NANOSECONDS,
	}
	MetaConnFailures = metric.Metadata{
		Name:        "sql.conn.failures",
		Help:        "Number of sql conection failures",
		Measurement: "Connections",
		Unit:        metric.Unit_COUNT,
	}
	MetaPGWireCancelTotal = metric.Metadata{
		Name:        "sql.pgwire_cancel.total",
		Help:        "Counter of the number of pgwire query cancel requests",
		Measurement: "Requests",
		Unit:        metric.Unit_COUNT,
	}
	MetaPGWireCancelIgnored = metric.Metadata{
		Name:        "sql.pgwire_cancel.ignored",
		Help:        "Counter of the number of pgwire query cancel requests that were ignored due to rate limiting",
		Measurement: "Requests",
		Unit:        metric.Unit_COUNT,
	}
	MetaPGWireCancelSuccessful = metric.Metadata{
		Name:        "sql.pgwire_cancel.successful",
		Help:        "Counter of the number of pgwire query cancel requests that were successful",
		Measurement: "Requests",
		Unit:        metric.Unit_COUNT,
	}
)

const (
	// The below constants can occur during the first message a client
	// sends to the server. There are two categories: protocol version and
	// request code. The protocol version is (major version number << 16)
	// + minor version number. Request codes are (1234 << 16) + 5678 + N,
	// where N started at 0 and is increased by 1 for every new request
	// code added, which happens rarely during major or minor Postgres
	// releases.
	//
	// See: https://www.postgresql.org/docs/current/protocol-message-formats.html

	version30     = 196608   // (3 << 16) + 0
	versionCancel = 80877102 // (1234 << 16) + 5678
	versionSSL    = 80877103 // (1234 << 16) + 5679
	versionGSSENC = 80877104 // (1234 << 16) + 5680
)

// cancelMaxWait is the amount of time a draining server gives to sessions to
// react to cancellation and return before a forceful shutdown.
const cancelMaxWait = 1 * time.Second

// baseSQLMemoryBudget is the amount of memory pre-allocated in each connection.
var baseSQLMemoryBudget = envutil.EnvOrDefaultInt64("COCKROACH_BASE_SQL_MEMORY_BUDGET",
	int64(2.1*float64(mon.DefaultPoolAllocationSize)))

// connReservationBatchSize determines for how many connections memory
// is pre-reserved at once.
var connReservationBatchSize = 5

var (
	sslSupported   = []byte{'S'}
	sslUnsupported = []byte{'N'}
)

// cancelChanMap keeps track of channels that are closed after the associated
// cancellation function has been called and the cancellation has taken place.
type cancelChanMap map[chan struct{}]context.CancelFunc

// Server implements the server side of the PostgreSQL wire protocol.
type Server struct {
	AmbientCtx log.AmbientContext
	cfg        *base.Config
	SQLServer  *sql.Server
	execCfg    *sql.ExecutorConfig

	metrics ServerMetrics

	mu struct {
		syncutil.Mutex
		// connCancelMap entries represent connections started when the server
		// was not draining. Each value is a function that can be called to
		// cancel the associated connection. The corresponding key is a channel
		// that is closed when the connection is done.
		connCancelMap cancelChanMap
		// draining is set to true when the server starts draining the SQL layer.
		// When set to true, remaining SQL connections will be closed.
		// After the timeout set by server.shutdown.query_wait,
		// all connections will be closed regardless any queries in flight.
		draining bool
		// rejectNewConnections is set true when the server does not accept new
		// SQL connections, e.g. when the draining process enters the phase whose
		// duration is specified by the server.shutdown.connection_wait.
		rejectNewConnections bool
	}

	auth struct {
		syncutil.RWMutex
		conf        *hba.Conf
		identityMap *identmap.Conf
	}

	sqlMemoryPool *mon.BytesMonitor
	connMonitor   *mon.BytesMonitor

	// testing{Conn,Auth}LogEnabled is used in unit tests in this
	// package to force-enable conn/auth logging without dancing around
	// the asynchronicity of cluster settings.
	testingConnLogEnabled int32
	testingAuthLogEnabled int32

	// trustClientProvidedRemoteAddr indicates whether the server should honor
	// a `crdb:remote_addr` status parameter provided by the client during
	// session authentication. This status parameter can be set by SQL proxies
	// to feed the "real" client address, where otherwise the CockroachDB SQL
	// server would only see the address of the proxy.
	//
	// This setting is security-sensitive and should not be enabled
	// without a SQL proxy that carefully scrubs any client-provided
	// `crdb:remote_addr` field. In particular, this setting should never
	// be set when there is no SQL proxy at all. Otherwise, a malicious
	// client could use this field to pretend being from another address
	// than its own and defeat the HBA rules.
	//
	// TODO(knz,ben): It would be good to have something more specific
	// than a boolean, i.e. to accept the provided address only from
	// certain peer IPs, or with certain certificates. (could it be a
	// special hba.conf directive?)
	trustClientProvidedRemoteAddr syncutil.AtomicBool
}

// ServerMetrics is the set of metrics for the pgwire server.
type ServerMetrics struct {
	BytesInCount                *metric.Counter
	BytesOutCount               *metric.Counter
	Conns                       *metric.Gauge
	NewConns                    *metric.Counter
	ConnLatency                 *metric.Histogram
	ConnFailures                *metric.Counter
	PGWireCancelTotalCount      *metric.Counter
	PGWireCancelIgnoredCount    *metric.Counter
	PGWireCancelSuccessfulCount *metric.Counter
	ConnMemMetrics              sql.BaseMemoryMetrics
	SQLMemMetrics               sql.MemoryMetrics
}

func makeServerMetrics(
	sqlMemMetrics sql.MemoryMetrics, histogramWindow time.Duration,
) ServerMetrics {
	return ServerMetrics{
		BytesInCount:                metric.NewCounter(MetaBytesIn),
		BytesOutCount:               metric.NewCounter(MetaBytesOut),
		Conns:                       metric.NewGauge(MetaConns),
		NewConns:                    metric.NewCounter(MetaNewConns),
		ConnLatency:                 metric.NewLatency(MetaConnLatency, histogramWindow),
		ConnFailures:                metric.NewCounter(MetaConnFailures),
		PGWireCancelTotalCount:      metric.NewCounter(MetaPGWireCancelTotal),
		PGWireCancelIgnoredCount:    metric.NewCounter(MetaPGWireCancelIgnored),
		PGWireCancelSuccessfulCount: metric.NewCounter(MetaPGWireCancelSuccessful),
		ConnMemMetrics:              sql.MakeBaseMemMetrics("conns", histogramWindow),
		SQLMemMetrics:               sqlMemMetrics,
	}
}

// noteworthySQLMemoryUsageBytes is the minimum size tracked by the
// client SQL pool before the pool start explicitly logging overall
// usage growth in the log.
var noteworthySQLMemoryUsageBytes = envutil.EnvOrDefaultInt64("COCKROACH_NOTEWORTHY_SQL_MEMORY_USAGE", 100*1024*1024)

// noteworthyConnMemoryUsageBytes is the minimum size tracked by the
// connection monitor before the monitor start explicitly logging overall
// usage growth in the log.
var noteworthyConnMemoryUsageBytes = envutil.EnvOrDefaultInt64("COCKROACH_NOTEWORTHY_CONN_MEMORY_USAGE", 2*1024*1024)

// MakeServer creates a Server.
//
// Start() needs to be called on the Server so it begins processing.
func MakeServer(
	ambientCtx log.AmbientContext,
	cfg *base.Config,
	st *cluster.Settings,
	sqlMemMetrics sql.MemoryMetrics,
	parentMemoryMonitor *mon.BytesMonitor,
	histogramWindow time.Duration,
	executorConfig *sql.ExecutorConfig,
) *Server {
	server := &Server{
		AmbientCtx: ambientCtx,
		cfg:        cfg,
		execCfg:    executorConfig,
		metrics:    makeServerMetrics(sqlMemMetrics, histogramWindow),
	}
	server.sqlMemoryPool = mon.NewMonitor("sql",
		mon.MemoryResource,
		// Note that we don't report metrics on this monitor. The reason for this is
		// that we report metrics on the sum of all the child monitors of this pool.
		// This monitor is the "main sql" monitor. It's a child of the root memory
		// monitor. Its children are the sql monitors for each new connection. The
		// sum of those children, plus the extra memory in the "conn" monitor below,
		// is more than enough metrics information about the monitors.
		nil, /* curCount */
		nil, /* maxHist */
		0, noteworthySQLMemoryUsageBytes, st)
	server.sqlMemoryPool.Start(context.Background(), parentMemoryMonitor, mon.BoundAccount{})
	server.SQLServer = sql.NewServer(executorConfig, server.sqlMemoryPool)

	// TODO(knz,ben): Use a cluster setting for this.
	server.trustClientProvidedRemoteAddr.Set(trustClientProvidedRemoteAddrOverride)

	server.connMonitor = mon.NewMonitor("conn",
		mon.MemoryResource,
		server.metrics.ConnMemMetrics.CurBytesCount,
		server.metrics.ConnMemMetrics.MaxBytesHist,
		int64(connReservationBatchSize)*baseSQLMemoryBudget, noteworthyConnMemoryUsageBytes, st)
	server.connMonitor.Start(context.Background(), server.sqlMemoryPool, mon.BoundAccount{})

	server.mu.Lock()
	server.mu.connCancelMap = make(cancelChanMap)
	server.mu.Unlock()

	connAuthConf.SetOnChange(&st.SV, func(ctx context.Context) {
		loadLocalHBAConfigUponRemoteSettingChange(
			ambientCtx.AnnotateCtx(context.Background()), server, st)
	})
	connIdentityMapConf.SetOnChange(&st.SV, func(ctx context.Context) {
		loadLocalIdentityMapUponRemoteSettingChange(
			ambientCtx.AnnotateCtx(context.Background()), server, st)
	})

	return server
}

// BytesOut returns the total number of bytes transmitted from this server.
func (s *Server) BytesOut() uint64 {
	return uint64(s.metrics.BytesOutCount.Count())
}

// AnnotateCtxForIncomingConn annotates the provided context with a
// tag that reports the peer's address. In the common case, the
// context is annotated with a "client" tag. When the server is
// configured to recognize client-specified remote addresses, it is
// annotated with a "peer" tag and the "client" tag is added later
// when the session is set up.
func (s *Server) AnnotateCtxForIncomingConn(ctx context.Context, conn net.Conn) context.Context {
	tag := "client"
	if s.trustClientProvidedRemoteAddr.Get() {
		tag = "peer"
	}
	return logtags.AddTag(ctx, tag, conn.RemoteAddr().String())
}

// Match returns true if rd appears to be a Postgres connection.
func Match(rd io.Reader) bool {
	buf := pgwirebase.MakeReadBuffer()
	_, err := buf.ReadUntypedMsg(rd)
	if err != nil {
		return false
	}
	version, err := buf.GetUint32()
	if err != nil {
		return false
	}
	return version == version30 || version == versionSSL || version == versionCancel || version == versionGSSENC
}

// Start makes the Server ready for serving connections.
func (s *Server) Start(ctx context.Context, stopper *stop.Stopper) {
	s.SQLServer.Start(ctx, stopper)
}

// IsDraining returns true if the server is not currently accepting
// connections.
func (s *Server) IsDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mu.draining
}

// Metrics returns the set of metrics structs.
func (s *Server) Metrics() (res []interface{}) {
	return []interface{}{
		&s.metrics,
		&s.SQLServer.Metrics.StartedStatementCounters,
		&s.SQLServer.Metrics.ExecutedStatementCounters,
		&s.SQLServer.Metrics.EngineMetrics,
		&s.SQLServer.Metrics.GuardrailMetrics,
		&s.SQLServer.InternalMetrics.StartedStatementCounters,
		&s.SQLServer.InternalMetrics.ExecutedStatementCounters,
		&s.SQLServer.InternalMetrics.EngineMetrics,
		&s.SQLServer.InternalMetrics.GuardrailMetrics,
		&s.SQLServer.ServerMetrics.StatsMetrics,
		&s.SQLServer.ServerMetrics.ContentionSubsystemMetrics,
	}
}

// Drain prevents new connections from being served and waits the duration of
// queryWait for open connections to terminate before canceling them.
// An error will be returned when connections that have been canceled have not
// responded to this cancellation and closed themselves in time. The server
// will remain in draining state, though open connections may continue to
// exist.
// The RFC on drain modes has more information regarding the specifics of
// what will happen to connections in different states:
// https://github.com/cockroachdb/cockroach/blob/master/docs/RFCS/20160425_drain_modes.md
//
// The reporter callback, if non-nil, is called on a best effort basis
// to report work that needed to be done and which may or may not have
// been done by the time this call returns. See the explanation in
// pkg/server/drain.go for details.
func (s *Server) Drain(
	ctx context.Context,
	queryWait time.Duration,
	reporter func(int, redact.SafeString),
	stopper *stop.Stopper,
) error {
	return s.drainImpl(ctx, queryWait, cancelMaxWait, reporter, stopper)
}

// Undrain switches the server back to the normal mode of operation in which
// connections are accepted.
func (s *Server) Undrain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setRejectNewConnectionsLocked(false)
	s.setDrainingLocked(false)
}

// setDrainingLocked sets the server's draining state and returns whether the
// state changed (i.e. drain != s.mu.draining). s.mu must be locked when
// setDrainingLocked is called.
func (s *Server) setDrainingLocked(drain bool) bool {
	if s.mu.draining == drain {
		return false
	}
	s.mu.draining = drain
	return true
}

// setRejectNewConnectionsLocked sets the server's rejectNewConnections state.
// s.mu must be locked when setRejectNewConnectionsLocked is called.
func (s *Server) setRejectNewConnectionsLocked(rej bool) {
	s.mu.rejectNewConnections = rej
}

// GetConnCancelMapLen returns the length of connCancelMap of the server.
// This is a helper function when the server waits the SQL connections to be
// closed. During this period, the server listens to the status of all
// connections, and early exits this draining phase if there remains no active
// SQL connections.
func (s *Server) GetConnCancelMapLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mu.connCancelMap)
}

// WaitForSQLConnsToClose waits for the client to close all SQL connections for the
// duration of connectionWait.
// With this phase, the node starts rejecting SQL connections, and as
// soon as all existing SQL connections are closed, the server early exits this
// draining phase.
func (s *Server) WaitForSQLConnsToClose(
	ctx context.Context, connectionWait time.Duration, stopper *stop.Stopper,
) error {
	// If we're already draining the SQL connections, we don't need to wait again.
	if s.IsDraining() {
		return nil
	}

	s.mu.Lock()
	s.setRejectNewConnectionsLocked(true)
	s.mu.Unlock()

	if connectionWait == 0 {
		return nil
	}

	log.Ops.Info(ctx, "waiting for clients to close existing SQL connections")

	timer := time.NewTimer(connectionWait)
	defer timer.Stop()

	_, allConnsDone, quitWaitingForConns := s.waitConnsDone()
	defer close(quitWaitingForConns)

	select {
	// Connection wait times out.
	case <-time.After(connectionWait):
		log.Ops.Warningf(ctx,
			"%d connections remain after waiting %s; proceeding to drain SQL connections",
			s.GetConnCancelMapLen(),
			connectionWait,
		)
	case <-allConnsDone:
	case <-ctx.Done():
		return ctx.Err()
	case <-stopper.ShouldQuiesce():
		return context.Canceled
	}

	return nil
}

// waitConnsDone returns a copy of s.mu.connCancelMap, and a channel that
// will be closed once all sql connections are closed, or the server quits
// waiting for connections, whichever earlier.
func (s *Server) waitConnsDone() (cancelChanMap, chan struct{}, chan struct{}) {
	connCancelMap := func() cancelChanMap {
		s.mu.Lock()
		defer s.mu.Unlock()
		connCancelMap := make(cancelChanMap)
		for done, cancel := range s.mu.connCancelMap {
			connCancelMap[done] = cancel
		}
		return connCancelMap
	}()

	allConnsDone := make(chan struct{}, 1)

	quitWaitingForConns := make(chan struct{}, 1)

	go func() {
		defer close(allConnsDone)

		for done := range connCancelMap {
			select {
			case <-done:
			case <-quitWaitingForConns:
				return
			}
		}
	}()

	return connCancelMap, allConnsDone, quitWaitingForConns
}

// drainImpl drains the SQL clients.
//
// The queryWait duration is used to wait on clients to
// self-disconnect after their session has been canceled. The
// cancelWait is used to wait after the queryWait timer has expired
// and there are still clients connected, and their context.Context is
// canceled.
//
// The reporter callback, if non-nil, is called on a best effort basis
// to report work that needed to be done and which may or may not have
// been done by the time this call returns. See the explanation in
// pkg/server/drain.go for details.
func (s *Server) drainImpl(
	ctx context.Context,
	queryWait time.Duration,
	cancelWait time.Duration,
	reporter func(int, redact.SafeString),
	stopper *stop.Stopper,
) error {

	s.mu.Lock()
	if !s.setDrainingLocked(true) {
		// We are already draining.
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// If there is no open SQL connections to drain, just return.
	if s.GetConnCancelMapLen() == 0 {
		return nil
	}

	log.Ops.Info(ctx, "starting draining SQL connections")

	// Spin off a goroutine that waits for all connections to signal that they
	// are done and reports it on allConnsDone. The main goroutine signals this
	// goroutine to stop work through quitWaitingForConns.

	// This s.waitConnsDone function returns a copy of s.mu.connCancelMap if there
	// are any active connections to cancel. We will only attempt to cancel
	// connections that were active at the moment the draining switch happened.
	// It is enough to do this because:
	// 1) If no new connections are added to the original map all connections
	// will be canceled.
	// 2) If new connections are added to the original map, it follows that they
	// were added when s.mu.draining = false, thus not requiring cancellation.
	// These connections are not our responsibility and will be handled when the
	// server starts draining again.
	connCancelMap, allConnsDone, quitWaitingForConns := s.waitConnsDone()
	defer close(quitWaitingForConns)

	if reporter != nil {
		// Report progress to the Drain RPC.
		reporter(len(connCancelMap), "SQL clients")
	}

	// Wait for connections to finish up their queries for the duration of queryWait.
	select {
	case <-time.After(queryWait):
		log.Ops.Warningf(ctx, "canceling all sessions after waiting %s", queryWait)
	case <-allConnsDone:
	case <-ctx.Done():
		return ctx.Err()
	case <-stopper.ShouldQuiesce():
		return context.Canceled
	}

	// Cancel the contexts of all sessions if the server is still in draining
	// mode.
	if stop := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.mu.draining {
			return true
		}
		for _, cancel := range connCancelMap {
			// There is a possibility that different calls to SetDraining have
			// overlapping connCancelMaps, but context.CancelFunc calls are
			// idempotent.
			cancel()
		}
		return false
	}(); stop {
		return nil
	}

	select {
	case <-time.After(cancelWait):
		return errors.Errorf("some sessions did not respond to cancellation within %s", cancelWait)
	case <-allConnsDone:
	}
	return nil
}

// SocketType indicates the connection type. This is an optimization to
// prevent a comparison against conn.LocalAddr().Network().
type SocketType bool

const (
	// SocketTCP is used for TCP sockets. The standard.
	SocketTCP SocketType = true
	// SocketUnix is used for unix datagram sockets.
	SocketUnix SocketType = false
)

func (s SocketType) asConnType() (hba.ConnType, error) {
	switch s {
	case SocketTCP:
		return hba.ConnHostNoSSL, nil
	case SocketUnix:
		return hba.ConnLocal, nil
	default:
		return 0, errors.AssertionFailedf("unimplemented socket type: %v", errors.Safe(s))
	}
}

func (s *Server) connLogEnabled() bool {
	return atomic.LoadInt32(&s.testingConnLogEnabled) != 0 || logConnAuth.Get(&s.execCfg.Settings.SV)
}

// TestingEnableConnLogging is exported for use in tests.
func (s *Server) TestingEnableConnLogging() {
	atomic.StoreInt32(&s.testingConnLogEnabled, 1)
}

// TestingEnableAuthLogging is exported for use in tests.
func (s *Server) TestingEnableAuthLogging() {
	atomic.StoreInt32(&s.testingAuthLogEnabled, 1)
}

// ServeConn serves a single connection, driving the handshake process and
// delegating to the appropriate connection type.
//
// The socketType argument is an optimization to avoid a string
// compare on conn.LocalAddr().Network(). When the socket type is
// unix datagram (local filesystem), SSL negotiation is disabled
// even when the server is running securely with certificates.
// This has the effect of forcing password auth, also in a way
// compatible with postgres.
//
// An error is returned if the initial handshake of the connection fails.
func (s *Server) ServeConn(ctx context.Context, conn net.Conn, socketType SocketType) (err error) {
	defer func() {
		if err != nil {
			s.metrics.ConnFailures.Inc(1)
		}
	}()

	ctx, rejectNewConnections, onCloseFn := s.registerConn(ctx)
	defer onCloseFn()

	connDetails := eventpb.CommonConnectionDetails{
		InstanceID:    int32(s.execCfg.NodeID.SQLInstanceID()),
		Network:       conn.RemoteAddr().Network(),
		RemoteAddress: conn.RemoteAddr().String(),
	}

	// Some bookkeeping, for security-minded administrators.
	// This registers the connection to the authentication log.
	connStart := timeutil.Now()
	if s.connLogEnabled() {
		ev := &eventpb.ClientConnectionStart{
			CommonEventDetails:      eventpb.CommonEventDetails{Timestamp: connStart.UnixNano()},
			CommonConnectionDetails: connDetails,
		}
		log.StructuredEvent(ctx, ev)
	}
	defer func() {
		// The duration of the session is logged at the end so that the
		// reader of the log file can know how much to look back in time
		// to find when the connection was opened. This is important
		// because the log files may have been rotated since.
		if s.connLogEnabled() {
			endTime := timeutil.Now()
			ev := &eventpb.ClientConnectionEnd{
				CommonEventDetails:      eventpb.CommonEventDetails{Timestamp: endTime.UnixNano()},
				CommonConnectionDetails: connDetails,
				Duration:                endTime.Sub(connStart).Nanoseconds(),
			}
			log.StructuredEvent(ctx, ev)
		}
	}()

	// In any case, first check the command in the start-up message.
	//
	// We're assuming that a client is not willing/able to receive error
	// packets before we drain that message.
	version, buf, err := s.readVersion(conn)
	if err != nil {
		return err
	}

	switch version {
	case versionCancel:
		// The cancel message is rather peculiar: it is sent without
		// authentication, always over an unencrypted channel.
		s.handleCancel(ctx, conn, &buf)
		return nil

	case versionGSSENC:
		// This is a request for an unsupported feature: GSS encryption.
		// https://github.com/cockroachdb/cockroach/issues/52184
		//
		// Ensure the right SQLSTATE is sent to the SQL client.
		err := pgerror.New(pgcode.ProtocolViolation, "GSS encryption is not yet supported")
		// Annotate a telemetry key. These objects
		// are treated specially by sendErr: they increase a
		// telemetry counter to indicate an attempt was made
		// to use this feature.
		err = errors.WithTelemetry(err, "#52184")
		return s.sendErr(ctx, conn, err)
	}

	// If the server is shutting down, terminate the connection early.
	if rejectNewConnections {
		log.Ops.Info(ctx, "rejecting new connection while server is draining")
		return s.sendErr(ctx, conn, newAdminShutdownErr(ErrDrainingNewConn))
	}

	// Compute the initial connType.
	connType, err := socketType.asConnType()
	if err != nil {
		return err
	}

	// If the client requests SSL, upgrade the connection to use TLS.
	var clientErr error
	conn, connType, version, clientErr, err = s.maybeUpgradeToSecureConn(ctx, conn, connType, version, &buf)
	if err != nil {
		return err
	}
	if clientErr != nil {
		return s.sendErr(ctx, conn, clientErr)
	}
	sp := tracing.SpanFromContext(ctx)
	sp.SetTag("conn_type", attribute.StringValue(connType.String()))

	// What does the client want to do?
	switch version {
	case version30:
		// Normal SQL connection. Proceed normally below.

	case versionCancel:
		// The PostgreSQL protocol definition says that cancel payloads
		// must be sent *prior to upgrading the connection to use TLS*.
		// Yet, we've found clients in the wild that send the cancel
		// after the TLS handshake, for example at
		// https://github.com/cockroachlabs/support/issues/600.
		s.handleCancel(ctx, conn, &buf)
		return nil

	default:
		// We don't know this protocol.
		err := pgerror.Newf(pgcode.ProtocolViolation, "unknown protocol version %d", version)
		err = errors.WithTelemetry(err, fmt.Sprintf("protocol-version-%d", version))
		return s.sendErr(ctx, conn, err)
	}

	// Reserve some memory for this connection using the server's monitor. This
	// reduces pressure on the shared pool because the server monitor allocates in
	// chunks from the shared pool and these chunks should be larger than
	// baseSQLMemoryBudget.
	reserved := s.connMonitor.MakeBoundAccount()
	if err := reserved.Grow(ctx, baseSQLMemoryBudget); err != nil {
		return errors.Wrapf(err, "unable to pre-allocate %d bytes for this connection",
			baseSQLMemoryBudget)
	}

	// Load the client-provided session parameters.
	var sArgs sql.SessionArgs
	if sArgs, err = parseClientProvidedSessionParameters(ctx, &s.execCfg.Settings.SV, &buf,
		conn.RemoteAddr(), s.trustClientProvidedRemoteAddr.Get()); err != nil {
		reserved.Close(ctx)
		return s.sendErr(ctx, conn, err)
	}

	// Populate the client address field in the context tags and the
	// shared struct for structured logging.
	// Only now do we know the remote client address for sure (it may have
	// been overridden by a status parameter).
	connDetails.RemoteAddress = sArgs.RemoteAddr.String()
	ctx = logtags.AddTag(ctx, "client", log.SafeOperational(connDetails.RemoteAddress))
	sp.SetTag("client", attribute.StringValue(connDetails.RemoteAddress))

	// If a test is hooking in some authentication option, load it.
	var testingAuthHook func(context.Context) error
	if k := s.execCfg.PGWireTestingKnobs; k != nil {
		testingAuthHook = k.AuthHook
	}

	hbaConf, identMap := s.GetAuthenticationConfiguration()

	// Defer the rest of the processing to the connection handler.
	// This includes authentication.
	s.serveConn(
		ctx, conn, sArgs,
		reserved,
		connStart,
		authOptions{
			connType:        connType,
			connDetails:     connDetails,
			insecure:        s.cfg.Insecure,
			ie:              s.execCfg.InternalExecutor,
			auth:            hbaConf,
			identMap:        identMap,
			testingAuthHook: testingAuthHook,
		},
	)
	return nil
}

// handleCancel handles a pgwire query cancellation request. Note that the
// request is unauthenticated. To mitigate the security risk (i.e., a
// malicious actor spamming this endpoint with random data to try to cancel
// a query), the logic is rate-limited by a semaphore. Refer to the comments
// in the pgwirecancel package for more information.
//
// This function does not return an error, so the caller (and possible
// attacker) will not know if the cancellation attempt succeeded. Errors are
// logged so that an operator can be aware of any possibly malicious requests.
func (s *Server) handleCancel(ctx context.Context, conn net.Conn, buf *pgwirebase.ReadBuffer) {
	telemetry.Inc(sqltelemetry.CancelRequestCounter)
	s.metrics.PGWireCancelTotalCount.Inc(1)

	resp, err := func() (*serverpb.CancelQueryByKeyResponse, error) {
		backendKeyDataBits, err := buf.GetUint64()
		// The connection that issued the cancel is not a SQL session -- it's an
		// entirely new connection that's created just to send the cancel. We close
		// the connection as soon as possible after reading the data, since there
		// is nothing to send back to the client.
		_ = conn.Close()
		if err != nil {
			return nil, err
		}
		cancelKey := pgwirecancel.BackendKeyData(backendKeyDataBits)
		// The request is forwarded to the appropriate node.
		req := &serverpb.CancelQueryByKeyRequest{
			SQLInstanceID:  cancelKey.GetSQLInstanceID(),
			CancelQueryKey: cancelKey,
		}
		resp, err := s.execCfg.SQLStatusServer.CancelQueryByKey(ctx, req)
		if resp != nil && len(resp.Error) > 0 {
			err = errors.CombineErrors(err, errors.Newf("error from CancelQueryByKeyResponse: %s", resp.Error))
		}
		return resp, err
	}()

	if resp != nil && resp.Canceled {
		s.metrics.PGWireCancelSuccessfulCount.Inc(1)
	} else if err != nil {
		if respStatus := status.Convert(err); respStatus.Code() == codes.ResourceExhausted {
			s.metrics.PGWireCancelIgnoredCount.Inc(1)
		}
		log.Sessions.Warningf(ctx, "unexpected while handling pgwire cancellation request: %v", err)
	}
}

// parseClientProvidedSessionParameters reads the incoming k/v pairs
// in the startup message into a sql.SessionArgs struct.
func parseClientProvidedSessionParameters(
	ctx context.Context,
	sv *settings.Values,
	buf *pgwirebase.ReadBuffer,
	origRemoteAddr net.Addr,
	trustClientProvidedRemoteAddr bool,
) (sql.SessionArgs, error) {
	args := sql.SessionArgs{
		SessionDefaults:             make(map[string]string),
		CustomOptionSessionDefaults: make(map[string]string),
		RemoteAddr:                  origRemoteAddr,
	}
	foundBufferSize := false

	for {
		// Read a key-value pair from the client.
		key, err := buf.GetString()
		if err != nil {
			return sql.SessionArgs{}, pgerror.Wrap(
				err, pgcode.ProtocolViolation,
				"error reading option key",
			)
		}
		if len(key) == 0 {
			// End of parameter list.
			break
		}
		value, err := buf.GetString()
		if err != nil {
			return sql.SessionArgs{}, pgerror.Wrapf(
				err, pgcode.ProtocolViolation,
				"error reading option value for key %q", key,
			)
		}

		// Case-fold for the key for easier comparison.
		key = strings.ToLower(key)

		// Load the parameter.
		switch key {
		case "user":
			// In CockroachDB SQL, unlike in PostgreSQL, usernames are
			// case-insensitive. Therefore we need to normalize the username
			// here, so that further lookups for authentication have the correct
			// identifier.
			args.User, _ = security.MakeSQLUsernameFromUserInput(value, security.UsernameValidation)
			// IsSuperuser will get updated later when we load the user's session
			// initialization information.
			args.IsSuperuser = args.User.IsRootUser()

		case "crdb:session_revival_token_base64":
			token, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				return sql.SessionArgs{}, pgerror.Wrapf(
					err, pgcode.ProtocolViolation,
					"%s", key,
				)
			}
			args.SessionRevivalToken = token

		case "results_buffer_size":
			if args.ConnResultsBufferSize, err = humanizeutil.ParseBytes(value); err != nil {
				return sql.SessionArgs{}, errors.WithSecondaryError(
					pgerror.Newf(pgcode.ProtocolViolation,
						"error parsing results_buffer_size option value '%s' as bytes", value), err)
			}
			if args.ConnResultsBufferSize < 0 {
				return sql.SessionArgs{}, pgerror.Newf(pgcode.ProtocolViolation,
					"results_buffer_size option value '%s' cannot be negative", value)
			}
			foundBufferSize = true

		case "crdb:remote_addr":
			if !trustClientProvidedRemoteAddr {
				return sql.SessionArgs{}, pgerror.Newf(pgcode.ProtocolViolation,
					"server not configured to accept remote address override (requested: %q)", value)
			}

			hostS, portS, err := net.SplitHostPort(value)
			if err != nil {
				return sql.SessionArgs{}, pgerror.Wrap(
					err, pgcode.ProtocolViolation,
					"invalid address format",
				)
			}
			port, err := strconv.Atoi(portS)
			if err != nil {
				return sql.SessionArgs{}, pgerror.Wrap(
					err, pgcode.ProtocolViolation,
					"remote port is not numeric",
				)
			}
			ip := net.ParseIP(hostS)
			if ip == nil {
				return sql.SessionArgs{}, pgerror.New(pgcode.ProtocolViolation,
					"remote address is not numeric")
			}
			args.RemoteAddr = &net.TCPAddr{IP: ip, Port: port}

		case "options":
			opts, err := parseOptions(value)
			if err != nil {
				return sql.SessionArgs{}, err
			}
			for _, opt := range opts {
				err = loadParameter(ctx, opt.key, opt.value, &args)
				if err != nil {
					return sql.SessionArgs{}, pgerror.Wrapf(err, pgerror.GetPGCode(err), "options")
				}
			}
		default:
			err = loadParameter(ctx, key, value, &args)
			if err != nil {
				return sql.SessionArgs{}, err
			}
		}
	}

	if !foundBufferSize && sv != nil {
		// The client did not provide buffer_size; use the cluster setting as default.
		args.ConnResultsBufferSize = connResultsBufferSize.Get(sv)
	}

	// TODO(richardjcai): When connecting to the database, we'll want to
	// check for CONNECT privilege on the database. #59875.
	if _, ok := args.SessionDefaults["database"]; !ok {
		// CockroachDB-specific behavior: if no database is specified,
		// default to "defaultdb". In PostgreSQL this would be "postgres".
		args.SessionDefaults["database"] = catalogkeys.DefaultDatabaseName
	}

	// The client might override the application name,
	// which would prevent it from being counted in telemetry.
	// We've decided that this noise in the data is acceptable.
	if appName, ok := args.SessionDefaults["application_name"]; ok {
		if appName == catconstants.ReportableAppNamePrefix+catconstants.InternalSQLAppName {
			telemetry.Inc(sqltelemetry.CockroachShellCounter)
		}
	}

	return args, nil
}

func loadParameter(ctx context.Context, key, value string, args *sql.SessionArgs) error {
	key = strings.ToLower(key)
	exists, configurable := sql.IsSessionVariableConfigurable(key)

	switch {
	case exists && configurable:
		args.SessionDefaults[key] = value
	case sql.IsCustomOptionSessionVariable(key):
		args.CustomOptionSessionDefaults[key] = value
	case !exists:
		if _, ok := sql.UnsupportedVars[key]; ok {
			counter := sqltelemetry.UnimplementedClientStatusParameterCounter(key)
			telemetry.Inc(counter)
		}
		log.Warningf(ctx, "unknown configuration parameter: %q", key)

	case !configurable:
		return pgerror.Newf(pgcode.CantChangeRuntimeParam,
			"parameter %q cannot be changed", key)
	}
	return nil
}

// option represents an option argument passed in the connection URL.
type option struct {
	key   string
	value string
}

// parseOptions parses the given string into the options. The options must be
// separated by space and have one of the following patterns:
// '-c key=value', '-ckey=value', '--key=value'
func parseOptions(optionsString string) ([]option, error) {
	var res []option
	optionsRaw, err := url.QueryUnescape(optionsString)
	if err != nil {
		return nil, pgerror.Newf(pgcode.ProtocolViolation, "failed to unescape options %q", optionsString)
	}

	lastWasDashC := false
	opts := splitOptions(optionsRaw)

	for i := 0; i < len(opts); i++ {
		prefix := ""
		if len(opts[i]) > 1 {
			prefix = opts[i][:2]
		}

		switch {
		case opts[i] == "-c":
			lastWasDashC = true
			continue
		case lastWasDashC:
			lastWasDashC = false
			// if the last option was '-c' parse current option with no regard to
			// the prefix
			prefix = ""
		case prefix == "--" || prefix == "-c":
			lastWasDashC = false
		default:
			return nil, pgerror.Newf(pgcode.ProtocolViolation,
				"option %q is invalid, must have prefix '-c' or '--'", opts[i])
		}

		opt, err := splitOption(opts[i], prefix)
		if err != nil {
			return nil, err
		}
		res = append(res, opt)
	}
	return res, nil
}

// splitOptions slices the given string into substrings separated by space
// unless the space is escaped using backslashes '\\'. It also skips multiple
// subsequent spaces.
func splitOptions(options string) []string {
	var res []string
	var sb strings.Builder
	i := 0
	for i < len(options) {
		sb.Reset()
		// skip leading space
		for i < len(options) && unicode.IsSpace(rune(options[i])) {
			i++
		}
		if i == len(options) {
			break
		}

		lastWasEscape := false

		for i < len(options) {
			if unicode.IsSpace(rune(options[i])) && !lastWasEscape {
				break
			}
			if !lastWasEscape && options[i] == '\\' {
				lastWasEscape = true
			} else {
				lastWasEscape = false
				sb.WriteByte(options[i])
			}
			i++
		}

		res = append(res, sb.String())
	}

	return res
}

// splitOption splits the given opt argument into substrings separated by '='.
// It returns an error if the given option does not comply with the pattern
// "key=value" and the number of elements in the result is not two.
// splitOption removes the prefix from the key and replaces '-' with '_' so
// "--option-name=value" becomes [option_name, value].
func splitOption(opt, prefix string) (option, error) {
	kv := strings.Split(opt, "=")

	if len(kv) != 2 {
		return option{}, pgerror.Newf(pgcode.ProtocolViolation,
			"option %q is invalid, check '='", opt)
	}

	kv[0] = strings.TrimPrefix(kv[0], prefix)

	return option{key: strings.ReplaceAll(kv[0], "-", "_"), value: kv[1]}, nil
}

// Note: Usage of an env var here makes it possible to unconditionally
// enable this feature when cluster settings do not work reliably,
// e.g. in multi-tenant setups in v20.2. This override mechanism can
// be removed after all of CC is moved to use v21.1 or a version which
// supports cluster settings.
var trustClientProvidedRemoteAddrOverride = envutil.EnvOrDefaultBool("COCKROACH_TRUST_CLIENT_PROVIDED_SQL_REMOTE_ADDR", false)

// TestingSetTrustClientProvidedRemoteAddr is used in tests.
func (s *Server) TestingSetTrustClientProvidedRemoteAddr(b bool) func() {
	prev := s.trustClientProvidedRemoteAddr.Get()
	s.trustClientProvidedRemoteAddr.Set(b)
	return func() { s.trustClientProvidedRemoteAddr.Set(prev) }
}

// maybeUpgradeToSecureConn upgrades the connection to TLS/SSL if
// requested by the client, and available in the server configuration.
func (s *Server) maybeUpgradeToSecureConn(
	ctx context.Context,
	conn net.Conn,
	connType hba.ConnType,
	version uint32,
	buf *pgwirebase.ReadBuffer,
) (newConn net.Conn, newConnType hba.ConnType, newVersion uint32, clientErr, serverErr error) {
	// By default, this is a no-op.
	newConn = conn
	newConnType = connType
	newVersion = version
	var n int // byte counts

	if version != versionSSL {
		// The client did not require a SSL connection.

		// Insecure mode: nothing to say, nothing to do.
		// TODO(knz): Remove this condition - see
		// https://github.com/cockroachdb/cockroach/issues/53404
		if s.cfg.Insecure {
			return
		}

		// Secure mode: disallow if TCP and the user did not opt into
		// non-TLS SQL conns.
		if !s.cfg.AcceptSQLWithoutTLS && connType != hba.ConnLocal {
			clientErr = pgerror.New(pgcode.ProtocolViolation, ErrSSLRequired)
		}
		return
	}

	if connType == hba.ConnLocal {
		// No existing PostgreSQL driver ever tries to activate TLS over
		// a unix socket. But in case someone, sometime, somewhere, makes
		// that mistake, let them know that we don't want it.
		clientErr = pgerror.New(pgcode.ProtocolViolation,
			"cannot use SSL/TLS over local connections")
		return
	}

	// Protocol sanity check.
	if len(buf.Msg) > 0 {
		serverErr = errors.Errorf("unexpected data after SSLRequest: %q", buf.Msg)
		return
	}

	// The client has requested SSL. We're going to try and upgrade the
	// connection to use TLS/SSL.

	// Do we have a TLS configuration?
	tlsConfig, serverErr := s.execCfg.RPCContext.GetServerTLSConfig()
	if serverErr != nil {
		return
	}

	if tlsConfig == nil {
		// We don't have a TLS configuration available, so we can't honor
		// the client's request.
		n, serverErr = conn.Write(sslUnsupported)
		if serverErr != nil {
			return
		}
	} else {
		// We have a TLS configuration. Upgrade the connection.
		n, serverErr = conn.Write(sslSupported)
		if serverErr != nil {
			return
		}
		newConn = tls.Server(conn, tlsConfig)
		newConnType = hba.ConnHostSSL
	}
	s.metrics.BytesOutCount.Inc(int64(n))

	// Finally, re-read the version/command from the client.
	newVersion, *buf, serverErr = s.readVersion(newConn)
	return
}

// registerConn registers the incoming connection to the map of active connections,
// which can be canceled by a concurrent server drain. It also returns a boolean
// variable rejectConn, which shows if the server is rejecting new SQL
// connections.
//
// The onCloseFn() callback must be called at the end of the
// connection by the caller.
func (s *Server) registerConn(
	ctx context.Context,
) (newCtx context.Context, rejectNewConnections bool, onCloseFn func()) {
	onCloseFn = func() {}
	newCtx = ctx
	s.mu.Lock()
	rejectNewConnections = s.mu.rejectNewConnections
	if !rejectNewConnections {
		var cancel context.CancelFunc
		newCtx, cancel = contextutil.WithCancel(ctx)
		done := make(chan struct{})
		s.mu.connCancelMap[done] = cancel
		onCloseFn = func() {
			cancel()
			close(done)
			s.mu.Lock()
			delete(s.mu.connCancelMap, done)
			s.mu.Unlock()
		}
	}
	s.mu.Unlock()

	// If the server is rejecting new SQL connections, we will use the connection
	// only to send an error, so we don't count it in the stats. This makes sense
	// since DrainClient() waits for that number to drop to zero,
	// so we don't want it to oscillate unnecessarily.
	if !rejectNewConnections {
		s.metrics.NewConns.Inc(1)
		s.metrics.Conns.Inc(1)
		prevOnCloseFn := onCloseFn
		onCloseFn = func() { prevOnCloseFn(); s.metrics.Conns.Dec(1) }
	}
	return
}

// readVersion reads the start-up message, then returns the version
// code (first uint32 in message) and the buffer containing the rest
// of the payload.
func (s *Server) readVersion(
	conn io.Reader,
) (version uint32, buf pgwirebase.ReadBuffer, err error) {
	var n int
	buf = pgwirebase.MakeReadBuffer(
		pgwirebase.ReadBufferOptionWithClusterSettings(&s.execCfg.Settings.SV),
	)
	n, err = buf.ReadUntypedMsg(conn)
	if err != nil {
		return
	}
	version, err = buf.GetUint32()
	if err != nil {
		return
	}
	s.metrics.BytesInCount.Inc(int64(n))
	return
}

// sendErr sends errors to the client during the connection startup
// sequence. Later error sends during/after authentication are handled
// in conn.go.
func (s *Server) sendErr(ctx context.Context, conn net.Conn, err error) error {
	msgBuilder := newWriteBuffer(s.metrics.BytesOutCount)
	// We could, but do not, report server-side network errors while
	// trying to send the client error. This is because clients that
	// receive error payload are highly correlated with clients
	// disconnecting abruptly.
	_ /* err */ = writeErr(ctx, &s.execCfg.Settings.SV, err, msgBuilder, conn)
	_ = conn.Close()
	return err
}

func newAdminShutdownErr(msg string) error {
	return pgerror.New(pgcode.AdminShutdown, msg)
}
