package litefs

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/superfly/litefs/internal"
	"github.com/superfly/litefs/internal/chunk"
	"github.com/superfly/ltx"
	"golang.org/x/sync/errgroup"
)

// Default store settings.
const (
	DefaultReconnectDelay = 1 * time.Second
	DefaultDemoteDelay    = 10 * time.Second

	DefaultRetention                = 10 * time.Minute
	DefaultRetentionMonitorInterval = 1 * time.Minute

	DefaultHaltAcquireTimeout      = 5 * time.Second
	DefaultHaltLockTTL             = 30 * time.Second
	DefaultHaltLockMonitorInterval = 5 * time.Second

	DefaultBeginTimeout = 30 * time.Second
)

var ErrStoreClosed = fmt.Errorf("store closed")

// Store represents a collection of databases.
type Store struct {
	mu   sync.Mutex
	path string

	id          uint64 // unique node id
	dbs         map[string]*DB
	subscribers map[*Subscriber]struct{}

	isPrimary   bool          // if true, store is current primary
	primaryCh   chan struct{} // closed when primary loses leadership
	primaryInfo *PrimaryInfo  // contains info about the current primary
	candidate   bool          // if true, we are eligible to become the primary
	readyCh     chan struct{} // closed when primary found or acquired
	demoteCh    chan struct{} // closed when Demote() is called

	ctx    context.Context
	cancel context.CancelCauseFunc
	g      errgroup.Group

	logPrefix atomic.Value // combination of primary status + id

	// Client used to connect to other LiteFS instances.
	Client Client

	// Leaser manages the lease that controls leader election.
	Leaser Leaser

	// If true, LTX files are compressed using LZ4.
	Compress bool

	// Time to wait after disconnecting from the primary to reconnect.
	ReconnectDelay time.Duration

	// Time to wait after manually demoting trying to become primary again.
	DemoteDelay time.Duration

	// Length of time to retain LTX files.
	Retention                time.Duration
	RetentionMonitorInterval time.Duration

	// Time to wait to acquire the write lock after acquiring the HALT.
	HaltAcquireTimeout time.Duration

	// Max time to hold HALT lock and interval between expiration checks.
	HaltLockTTL             time.Duration
	HaltLockMonitorInterval time.Duration

	// Transaction timeouts.
	BeginTimeout time.Duration

	// Callback to notify kernel of file changes.
	Invalidator Invalidator

	// If true, computes and verifies the checksum of the entire database
	// after every transaction. Should only be used during testing.
	StrictVerify bool
}

// NewStore returns a new instance of Store.
func NewStore(path string, candidate bool) *Store {
	primaryCh := make(chan struct{})
	close(primaryCh)

	s := &Store{
		path: path,

		dbs: make(map[string]*DB),

		subscribers: make(map[*Subscriber]struct{}),
		candidate:   candidate,
		primaryCh:   primaryCh,
		readyCh:     make(chan struct{}),
		demoteCh:    make(chan struct{}),

		ReconnectDelay: DefaultReconnectDelay,
		DemoteDelay:    DefaultDemoteDelay,

		Retention:                DefaultRetention,
		RetentionMonitorInterval: DefaultRetentionMonitorInterval,

		HaltAcquireTimeout:      DefaultHaltAcquireTimeout,
		HaltLockTTL:             DefaultHaltLockTTL,
		HaltLockMonitorInterval: DefaultHaltLockMonitorInterval,
	}
	s.ctx, s.cancel = context.WithCancelCause(context.Background())
	s.logPrefix.Store("")

	return s
}

// Path returns underlying data directory.
func (s *Store) Path() string { return s.path }

// DBDir returns the folder that stores all databases.
func (s *Store) DBDir() string {
	return filepath.Join(s.path, "dbs")
}

// DBPath returns the folder that stores a single database.
func (s *Store) DBPath(name string) string {
	return filepath.Join(s.path, "dbs", name)
}

// ID returns the unique identifier for this instance. Available after Open().
// Persistent across restarts if underlying storage is persistent.
func (s *Store) ID() uint64 {
	return s.id
}

// LogPrefix returns the primary status and the store ID.
func (s *Store) LogPrefix() string {
	return s.logPrefix.Load().(string)
}

// Open initializes the store based on files in the data directory.
func (s *Store) Open() error {
	if s.Leaser == nil {
		return fmt.Errorf("leaser required")
	}

	if err := os.MkdirAll(s.path, 0777); err != nil {
		return err
	}

	if err := s.initID(); err != nil {
		return fmt.Errorf("init node id: %w", err)
	}

	if err := s.openDatabases(); err != nil {
		return fmt.Errorf("open databases: %w", err)
	}

	// Begin background replication monitor.
	s.g.Go(func() error { return s.monitorLease(s.ctx) })

	// Begin lock monitor.
	s.g.Go(func() error { return s.monitorHaltLock(s.ctx) })

	// Begin retention monitor.
	if s.RetentionMonitorInterval > 0 {
		s.g.Go(func() error { return s.monitorRetention(s.ctx) })
	}

	return nil
}

// initID initializes an identifier that is unique to this node.
func (s *Store) initID() error {
	filename := filepath.Join(s.path, "id")

	// Read existing ID from file, if it exists.
	if buf, err := os.ReadFile(filename); err != nil && !os.IsNotExist(err) {
		return err
	} else if err == nil {
		str := string(bytes.TrimSpace(buf))
		if len(str) > 16 {
			str = str[:16]
		}
		if s.id, err = strconv.ParseUint(str, 16, 64); err != nil {
			return fmt.Errorf("cannot parse id file: %q", str)
		}
		s.updateLogPrefix()
		return nil // existing ID
	}

	// Generate a new node ID if file doesn't exist.
	b := make([]byte, 16)
	if _, err := io.ReadFull(crand.Reader, b); err != nil {
		return fmt.Errorf("generate id: %w", err)
	}
	id := binary.BigEndian.Uint64(b)

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintf(f, "%016X\n", id); err != nil {
		return err
	} else if err := f.Sync(); err != nil {
		return err
	} else if err := f.Close(); err != nil {
		return err
	}

	s.id = id
	s.updateLogPrefix()

	return nil
}

func (s *Store) openDatabases() error {
	if err := os.MkdirAll(s.DBDir(), 0777); err != nil {
		return err
	}

	fis, err := os.ReadDir(s.DBDir())
	if err != nil {
		return fmt.Errorf("readdir: %w", err)
	}
	for _, fi := range fis {
		if err := s.openDatabase(fi.Name()); err != nil {
			return fmt.Errorf("open database(%q): %w", fi.Name(), err)
		}
	}

	// Update metrics.
	storeDBCountMetric.Set(float64(len(s.dbs)))

	return nil
}

func (s *Store) openDatabase(name string) error {
	// Instantiate and open database.
	db := NewDB(s, name, s.DBPath(name))
	if err := db.Open(); err != nil {
		return err
	}

	// Add to internal lookups.
	s.dbs[db.Name()] = db

	return nil
}

// Close signals for the store to shut down.
func (s *Store) Close() (retErr error) {
	s.cancel(ErrStoreClosed)
	retErr = s.g.Wait()

	// Release outstanding HALT locks.
	for _, db := range s.DBs() {
		haltLock := db.RemoteHaltLock()
		if haltLock == nil {
			continue
		}

		log.Printf("releasing halt lock on %q", db.Name())

		if err := db.ReleaseRemoteHaltLock(context.Background(), haltLock.ID); err != nil {
			log.Printf("cannot release halt lock on %q on shutdown", db.Name())
		}
	}

	return retErr
}

// ReadyCh returns a channel that is closed once the store has become primary
// or once it has connected to the primary.
func (s *Store) ReadyCh() chan struct{} {
	return s.readyCh
}

// markReady closes the ready channel if it hasn't already been closed.
func (s *Store) markReady() {
	select {
	case <-s.readyCh:
		return
	default:
		close(s.readyCh)
	}
}

// Demote instructs store to destroy its primary lease, if any.
// Store will wait momentarily before attempting to become primary again.
func (s *Store) Demote() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.demoteCh)
	s.demoteCh = make(chan struct{})
}

// IsPrimary returns true if store has a lease to be the primary.
func (s *Store) IsPrimary() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isPrimary
}

func (s *Store) setIsPrimary(v bool) {
	// Create a new channel to notify about primary loss when becoming primary.
	// Or close existing channel if we are losing our primary status.
	if s.isPrimary != v {
		if v {
			s.primaryCh = make(chan struct{})
		} else {
			close(s.primaryCh)
		}
	}

	// Update state.
	s.isPrimary = v

	s.updateLogPrefix()

	// Update metrics.
	if s.isPrimary {
		storeIsPrimaryMetric.Set(1)
	} else {
		storeIsPrimaryMetric.Set(0)
	}
}

func (s *Store) updateLogPrefix() {
	prefix := "r"
	if s.isPrimary {
		prefix = "P"
	}
	s.logPrefix.Store(fmt.Sprintf("%s/%s", prefix, FormatNodeID(s.id)))
}

// PrimaryCtx wraps ctx with another context that will cancel when no longer primary.
func (s *Store) PrimaryCtx(ctx context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return newPrimaryCtx(ctx, s.primaryCh)
}

// PrimaryInfo returns info about the current primary.
func (s *Store) PrimaryInfo() (isPrimary bool, info *PrimaryInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isPrimary, s.primaryInfo.Clone()
}

// Candidate returns true if store is eligible to be the primary.
func (s *Store) Candidate() bool {
	return s.candidate
}

// DBByName returns a database by name.
// Returns nil if the database does not exist.
func (s *Store) DB(name string) *DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dbs[name]
}

// DBs returns a list of databases.
func (s *Store) DBs() []*DB {
	s.mu.Lock()
	defer s.mu.Unlock()

	a := make([]*DB, 0, len(s.dbs))
	for _, db := range s.dbs {
		a = append(a, db)
	}
	return a
}

// CreateDB creates a new database with the given name. The returned file handle
// must be closed by the caller. Returns an error if a database with the same
// name already exists.
func (s *Store) CreateDB(name string) (db *DB, f *os.File, err error) {
	defer func() {
		TraceLog.Printf("[CreateDatabase(%s)]: %s", name, errorKeyValue(err))
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Verify database doesn't already exist.
	if _, ok := s.dbs[name]; ok {
		return nil, nil, ErrDatabaseExists
	}

	// Generate database directory with name file & empty database file.
	dbPath := s.DBPath(name)
	if err := os.MkdirAll(dbPath, 0777); err != nil {
		return nil, nil, err
	}

	f, err = os.OpenFile(filepath.Join(dbPath, "database"), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, 0666)
	if err != nil {
		return nil, nil, err
	}

	// Create new database instance and add to maps.
	db = NewDB(s, name, dbPath)
	if err := db.Open(); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	s.dbs[name] = db

	// Notify listeners of change.
	s.markDirty(name)

	// Update metrics
	storeDBCountMetric.Set(float64(len(s.dbs)))

	return db, f, nil
}

// CreateDBIfNotExists creates an empty database with the given name.
func (s *Store) CreateDBIfNotExists(name string) (*DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Exit if database with same name already exists.
	if db := s.dbs[name]; db != nil {
		return db, nil
	}

	// Generate database directory with name file & empty database file.
	dbPath := s.DBPath(name)
	if err := os.MkdirAll(dbPath, 0777); err != nil {
		return nil, err
	}

	if err := os.WriteFile(filepath.Join(dbPath, "database"), nil, 0666); err != nil {
		return nil, err
	}

	// Create new database instance and add to maps.
	db := NewDB(s, name, dbPath)
	if err := db.Open(); err != nil {
		return nil, err
	}
	s.dbs[name] = db

	// Notify listeners of change.
	s.markDirty(name)

	// Update metrics
	storeDBCountMetric.Set(float64(len(s.dbs)))

	return db, nil
}

// DropDB deletes an existing database with the given name.
func (s *Store) DropDB(ctx context.Context, name string) (err error) {
	defer func() {
		TraceLog.Printf("[DropDatabase(%s)]: %s", name, errorKeyValue(err))
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Look up database.
	db := s.dbs[name]
	if db == nil {
		return ErrDatabaseNotFound
	}

	// Remove data directory for the database.
	if err := os.RemoveAll(db.Path()); err != nil {
		return fmt.Errorf("remove db path: %w", err)
	}

	// Remove from lookup on store.
	delete(s.dbs, name)

	// Notify listeners of change.
	s.markDirty(name)

	// Update metrics
	storeDBCountMetric.Set(float64(len(s.dbs)))

	return nil
}

// PosMap returns a map of databases and their transactional position.
func (s *Store) PosMap() map[string]Pos {
	s.mu.Lock()
	defer s.mu.Unlock()

	m := make(map[string]Pos, len(s.dbs))
	for _, db := range s.dbs {
		m[db.Name()] = db.Pos()
	}
	return m
}

// Subscribe creates a new subscriber for store changes.
func (s *Store) Subscribe() *Subscriber {
	s.mu.Lock()
	defer s.mu.Unlock()

	sub := newSubscriber(s)
	s.subscribers[sub] = struct{}{}

	storeSubscriberCountMetric.Set(float64(len(s.subscribers)))
	return sub
}

// Unsubscribe removes a subscriber from the store.
func (s *Store) Unsubscribe(sub *Subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.subscribers, sub)
	storeSubscriberCountMetric.Set(float64(len(s.subscribers)))
}

// MarkDirty marks a database dirty on all subscribers.
func (s *Store) MarkDirty(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markDirty(name)
}

func (s *Store) markDirty(name string) {
	for sub := range s.subscribers {
		sub.MarkDirty(name)
	}
}

// monitorLease continuously handles either the leader lease or replicates from the primary.
func (s *Store) monitorLease(ctx context.Context) error {
	for {
		// Exit if store is closed.
		if err := ctx.Err(); err != nil {
			return nil
		}

		// Attempt to either obtain a primary lock or read the current primary.
		lease, info, err := s.acquireLeaseOrPrimaryInfo(ctx)
		if err == ErrNoPrimary && !s.candidate {
			log.Printf("%s: cannot find primary & ineligible to become primary, retrying: %s", FormatNodeID(s.id), err)
			sleepWithContext(ctx, s.ReconnectDelay)
			continue
		} else if err != nil {
			log.Printf("%s: cannot acquire lease or find primary, retrying: %s", FormatNodeID(s.id), err)
			sleepWithContext(ctx, s.ReconnectDelay)
			continue
		}

		// Monitor as primary if we have obtained a lease.
		if lease != nil {
			log.Printf("%s: primary lease acquired, advertising as %s", FormatNodeID(s.id), s.Leaser.AdvertiseURL())
			if err := s.monitorLeaseAsPrimary(ctx, lease); err != nil {
				log.Printf("%s: primary lease lost, retrying: %s", FormatNodeID(s.id), err)
			}
			if err := s.Recover(ctx); err != nil {
				log.Printf("%s: state change recovery error (primary): %s", FormatNodeID(s.id), err)
			}
			continue
		}

		// Monitor as replica if another primary already exists.
		log.Printf("%s: existing primary found (%s), connecting as replica", FormatNodeID(s.id), info.Hostname)
		if err := s.monitorLeaseAsReplica(ctx, info); err == nil {
			log.Printf("%s: disconnected from primary, retrying", FormatNodeID(s.id))
		} else {
			log.Printf("%s: disconnected from primary with error, retrying: %s", FormatNodeID(s.id), err)
		}
		if err := s.Recover(ctx); err != nil {
			log.Printf("%s: state change recovery error (replica): %s", FormatNodeID(s.id), err)
		}
		sleepWithContext(ctx, s.ReconnectDelay)
	}
}

func (s *Store) acquireLeaseOrPrimaryInfo(ctx context.Context) (Lease, *PrimaryInfo, error) {
	// Attempt to find an existing primary first.
	info, err := s.Leaser.PrimaryInfo(ctx)
	if err == ErrNoPrimary && !s.candidate {
		return nil, nil, err // no primary, not eligible to become primary
	} else if err != nil && err != ErrNoPrimary {
		return nil, nil, fmt.Errorf("fetch primary url: %w", err)
	} else if err == nil {
		return nil, &info, nil
	}

	// If no primary, attempt to become primary.
	lease, err := s.Leaser.Acquire(ctx)
	if err == ErrPrimaryExists {
		// passthrough and retry primary info fetch
	} else if err != nil {
		return nil, nil, fmt.Errorf("acquire lease: %w", err)
	} else if lease != nil {
		return lease, nil, nil
	}

	// If we raced to become primary and another node beat us, retry the fetch.
	info, err = s.Leaser.PrimaryInfo(ctx)
	if err != nil {
		return nil, nil, err
	}
	return nil, &info, nil
}

// monitorLeaseAsPrimary monitors & renews the current lease.
// NOTE: This code is borrowed from the consul/api's RenewPeriodic() implementation.
func (s *Store) monitorLeaseAsPrimary(ctx context.Context, lease Lease) error {
	const timeout = 1 * time.Second

	// Attempt to destroy lease when we exit this function.
	var demoted bool
	defer func() {
		log.Printf("%s: exiting primary, destroying lease", FormatNodeID(s.id))
		if err := lease.Close(); err != nil {
			log.Printf("%s: cannot remove lease: %s", FormatNodeID(s.id), err)
		}

		// Pause momentarily if this was a manual demotion.
		if demoted {
			log.Printf("%s: waiting for %s after demotion", FormatNodeID(s.id), s.DemoteDelay)
			sleepWithContext(ctx, s.DemoteDelay)
		}
	}()

	// Mark as the primary node while we're in this function.
	s.mu.Lock()
	s.setIsPrimary(true)
	demoteCh := s.demoteCh
	s.mu.Unlock()

	// Mark store as ready if we've obtained primary status.
	s.markReady()

	// Ensure that we are no longer marked as primary once we exit this function.
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.setIsPrimary(false)
	}()

	waitDur := lease.TTL() / 2

	for {
		select {
		case <-time.After(waitDur):
			// Attempt to renew the lease. If the lease is gone then we need to
			// just exit and we can start over or connect to the new primary.
			//
			// If we just have a connection error then we'll try to more
			// aggressively retry the renewal until we exceed TTL.
			if err := lease.Renew(ctx); err == ErrLeaseExpired {
				return err
			} else if err != nil {
				// If our next renewal will exceed TTL, exit now.
				if time.Since(lease.RenewedAt())+timeout > lease.TTL() {
					time.Sleep(timeout)
					return ErrLeaseExpired
				}

				// Otherwise log error and try again after a shorter period.
				log.Printf("%s: lease renewal error, retrying: %s", FormatNodeID(s.id), err)
				waitDur = time.Second
				continue
			}

			// Renewal was successful, restart with low frequency.
			waitDur = lease.TTL() / 2

		case <-demoteCh:
			demoted = true
			log.Printf("%s: node manually demoted", FormatNodeID(s.id))
			return nil

		case <-ctx.Done():
			return nil // release lease when we shut down
		}
	}
}

// monitorLeaseAsReplica tries to connect to the primary node and stream down changes.
func (s *Store) monitorLeaseAsReplica(ctx context.Context, info *PrimaryInfo) error {
	if s.Client == nil {
		return fmt.Errorf("no client set, skipping replica monitor")
	}

	// Store the URL of the primary while we're in this function.
	s.mu.Lock()
	s.primaryInfo = info
	s.mu.Unlock()

	// Clear the primary URL once we leave this function since we can no longer connect.
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.primaryInfo = nil
	}()

	posMap := s.PosMap()
	st, err := s.Client.Stream(ctx, info.AdvertiseURL, s.id, posMap)
	if err != nil {
		return fmt.Errorf("connect to primary: %s ('%s')", err, info.AdvertiseURL)
	}
	defer func() { _ = st.Close() }()

	for {
		frame, err := ReadStreamFrame(st)
		if err == io.EOF {
			return nil // clean disconnect
		} else if err != nil {
			return fmt.Errorf("next frame: %w", err)
		}

		switch frame := frame.(type) {
		case *LTXStreamFrame:
			if err := s.processLTXStreamFrame(ctx, frame, chunk.NewReader(st)); err != nil {
				return fmt.Errorf("process ltx stream frame: %w", err)
			}
		case *ReadyStreamFrame:
			// Mark store as ready once we've received an initial replication set.
			s.markReady()
		case *EndStreamFrame:
			// Server cleanly disconnected
			return nil
		case *DropDBStreamFrame:
			if err := s.processDropDBStreamFrame(ctx, frame); err != nil {
				return fmt.Errorf("process drop db stream frame: %w", err)
			}
		default:
			return fmt.Errorf("invalid stream frame type: 0x%02x", frame.Type())
		}
	}
}

// monitorRetention periodically enforces retention of LTX files on the databases.
func (s *Store) monitorRetention(ctx context.Context) error {
	ticker := time.NewTicker(s.RetentionMonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.EnforceRetention(ctx); err != nil {
				return err
			}
		}
	}
}

// monitorHaltLock periodically check all halt locks for expiration.
func (s *Store) monitorHaltLock(ctx context.Context) error {
	ticker := time.NewTicker(s.HaltLockMonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.EnforceHaltLockExpiration(ctx)
		}
	}
}

// EnforceHaltLockExpiration expires any overdue HALT locks.
func (s *Store) EnforceHaltLockExpiration(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, db := range s.dbs {
		db.EnforceHaltLockExpiration(ctx)
	}
}

// Recover forces a rollback (journal) or checkpoint (wal) on all open databases.
// This is done when switching the primary/replica state.
func (s *Store) Recover(ctx context.Context) (err error) {
	for _, db := range s.DBs() {
		if err := db.Recover(ctx); err != nil {
			return fmt.Errorf("db %q: %w", db.Name(), err)
		}
	}
	return nil
}

// EnforceRetention enforces retention of LTX files on all databases.
func (s *Store) EnforceRetention(ctx context.Context) (err error) {
	// Skip enforcement if not set.
	if s.Retention <= 0 {
		return nil
	}

	minTime := time.Now().Add(-s.Retention).UTC()

	for _, db := range s.DBs() {
		if e := db.EnforceRetention(ctx, minTime); err == nil {
			err = fmt.Errorf("cannot enforce retention on db %q: %w", db.Name(), e)
		}
	}
	return nil
}

func (s *Store) processLTXStreamFrame(ctx context.Context, frame *LTXStreamFrame, src io.Reader) (err error) {
	db, err := s.CreateDBIfNotExists(frame.Name)
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}

	hdr, data, err := ltx.DecodeHeader(src)
	if err != nil {
		return fmt.Errorf("peek ltx header: %w", err)
	}
	src = io.MultiReader(bytes.NewReader(data), src)

	TraceLog.Printf("%s [ProcessLTXStreamFrame.Begin(%s)]: txid=%s-%s, preApplyChecksum=%016x", s.LogPrefix(), db.Name(), ltx.FormatTXID(hdr.MinTXID), ltx.FormatTXID(hdr.MaxTXID), hdr.PreApplyChecksum)
	defer func() {
		TraceLog.Printf("%s [ProcessLTXStreamFrame.End(%s)]: %s", db.store.LogPrefix(), db.name, errorKeyValue(err))
	}()

	// Acquire lock unless we are waiting for a database position, in which case,
	// we already have the lock.
	guardSet, err := db.AcquireWriteLock(ctx, nil)
	if err != nil {
		return err
	}
	defer guardSet.Unlock()

	// Skip frame if it already occurred on this node. This can happen if the
	// replica node created the transaction and forwarded it to the primary.
	if hdr.NodeID == s.ID() {
		dec := ltx.NewDecoder(src)
		if err := dec.Verify(); err != nil {
			return fmt.Errorf("verify duplicate ltx file: %w", err)
		}
		if _, err := io.Copy(io.Discard, src); err != nil {
			return fmt.Errorf("discard ltx body: %w", err)
		}
		return nil
	}

	// If we receive an LTX file while holding the remote HALT lock then the
	// remote lock must have expired or been released so we can clear it locally.
	//
	// We also hold the local WRITE lock so a local write cannot be in-progress.
	if haltLock := db.RemoteHaltLock(); haltLock != nil {
		TraceLog.Printf("%s [ProcessLTXStreamFrame.Unhalt(%s)]: replica holds HALT lock but received LTX file, unsetting HALT lock", s.LogPrefix(), db.Name())
		if err := db.UnsetRemoteHaltLock(ctx, haltLock.ID); err != nil {
			return fmt.Errorf("release remote halt lock: %w", err)
		}
	}

	// Verify LTX file pre-apply checksum matches the current database position
	// unless this is a snapshot, which will overwrite all data.
	if !hdr.IsSnapshot() {
		expectedPos := Pos{
			TXID:              hdr.MinTXID - 1,
			PostApplyChecksum: hdr.PreApplyChecksum,
		}
		if pos := db.Pos(); pos != expectedPos {
			return fmt.Errorf("position mismatch on db %q: %s <> %s", db.Name(), pos, expectedPos)
		}
	}

	// Write LTX file to a temporary file and we'll atomically rename later.
	path := db.LTXPath(hdr.MinTXID, hdr.MaxTXID)
	tmpPath := fmt.Sprintf("%s.%d.tmp", path, rand.Int())
	defer func() { _ = os.Remove(tmpPath) }()

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("cannot create temp ltx file: %w", err)
	}
	defer func() { _ = f.Close() }()

	n, err := io.Copy(f, src)
	if err != nil {
		return fmt.Errorf("write ltx file: %w", err)
	} else if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync ltx file: %w", err)
	}

	// Atomically rename file.
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename ltx file: %w", err)
	} else if err := internal.Sync(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync ltx dir: %w", err)
	}

	// Update metrics
	dbLTXCountMetricVec.WithLabelValues(db.Name()).Inc()
	dbLTXBytesMetricVec.WithLabelValues(db.Name()).Set(float64(n))

	// Remove other LTX files after a snapshot.
	if hdr.IsSnapshot() {
		dir, file := filepath.Split(path)
		log.Printf("snapshot received for %q, removing other ltx files: %s", db.Name(), file)
		if err := removeFilesExcept(dir, file); err != nil {
			return fmt.Errorf("remove ltx after snapshot: %w", err)
		}
	}

	// Attempt to apply the LTX file to the database.
	if err := db.ApplyLTXNoLock(ctx, path); err != nil {
		return fmt.Errorf("apply ltx: %w", err)
	}

	return nil
}

func (s *Store) processDropDBStreamFrame(ctx context.Context, frame *DropDBStreamFrame) (err error) {
	if err := s.DropDB(ctx, frame.Name); err == ErrDatabaseNotFound {
		log.Printf("dropped database does not exist, skipping")
	} else if err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	return nil
}

// Expvar returns a variable for debugging output.
func (s *Store) Expvar() expvar.Var { return (*StoreVar)(s) }

var _ expvar.Var = (*StoreVar)(nil)

type StoreVar Store

func (v *StoreVar) String() string {
	s := (*Store)(v)
	m := &storeVarJSON{
		IsPrimary: s.IsPrimary(),
		Candidate: s.candidate,
		DBs:       make(map[string]*dbVarJSON),
	}

	for _, db := range s.DBs() {
		pos := db.Pos()

		dbJSON := &dbVarJSON{
			Name:     db.Name(),
			TXID:     ltx.FormatTXID(pos.TXID),
			Checksum: fmt.Sprintf("%016x", pos.PostApplyChecksum),
		}

		dbJSON.Locks.Pending = db.pendingLock.State().String()
		dbJSON.Locks.Shared = db.sharedLock.State().String()
		dbJSON.Locks.Reserved = db.reservedLock.State().String()

		dbJSON.Locks.Write = db.writeLock.State().String()
		dbJSON.Locks.Ckpt = db.ckptLock.State().String()
		dbJSON.Locks.Recover = db.recoverLock.State().String()
		dbJSON.Locks.Read0 = db.read0Lock.State().String()
		dbJSON.Locks.Read1 = db.read1Lock.State().String()
		dbJSON.Locks.Read2 = db.read2Lock.State().String()
		dbJSON.Locks.Read3 = db.read3Lock.State().String()
		dbJSON.Locks.Read4 = db.read4Lock.State().String()
		dbJSON.Locks.DMS = db.dmsLock.State().String()

		m.DBs[db.Name()] = dbJSON
	}

	b, err := json.Marshal(m)
	if err != nil {
		return "null"
	}
	return string(b)
}

type storeVarJSON struct {
	IsPrimary bool                  `json:"isPrimary"`
	Candidate bool                  `json:"candidate"`
	DBs       map[string]*dbVarJSON `json:"dbs"`
}

// Subscriber subscribes to changes to databases in the store.
//
// It implements a set of "dirty" databases instead of a channel of all events
// as clients can be slow and we don't want to cause channels to back up. It
// is the responsibility of the caller to determine the state changes which is
// usually just checking the position of the client versus the store's database.
type Subscriber struct {
	store *Store

	mu       sync.Mutex
	notifyCh chan struct{}
	dirtySet map[string]struct{}
}

// newSubscriber returns a new instance of Subscriber associated with a store.
func newSubscriber(store *Store) *Subscriber {
	s := &Subscriber{
		store:    store,
		notifyCh: make(chan struct{}, 1),
		dirtySet: make(map[string]struct{}),
	}
	return s
}

// Close removes the subscriber from the store.
func (s *Subscriber) Close() error {
	s.store.Unsubscribe(s)
	return nil
}

// NotifyCh returns a channel that receives a value when the dirty set has changed.
func (s *Subscriber) NotifyCh() <-chan struct{} { return s.notifyCh }

// MarkDirty marks a database ID as dirty.
func (s *Subscriber) MarkDirty(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirtySet[name] = struct{}{}

	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

// DirtySet returns a set of database IDs that have changed since the last call
// to DirtySet(). This call clears the set.
func (s *Subscriber) DirtySet() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	dirtySet := s.dirtySet
	s.dirtySet = make(map[string]struct{})
	return dirtySet
}

var _ context.Context = (*primaryCtx)(nil)

// primaryCtx represents a context that is marked done when the node loses its primary status.
type primaryCtx struct {
	parent    context.Context
	primaryCh chan struct{}
	done      chan struct{}
}

func newPrimaryCtx(parent context.Context, primaryCh chan struct{}) *primaryCtx {
	ctx := &primaryCtx{
		parent:    parent,
		primaryCh: primaryCh,
		done:      make(chan struct{}),
	}

	go func() {
		select {
		case <-ctx.primaryCh:
			close(ctx.done)
		case <-ctx.parent.Done():
			close(ctx.done)
		}
	}()

	return ctx
}

func (ctx *primaryCtx) Deadline() (deadline time.Time, ok bool) {
	return ctx.parent.Deadline()
}

func (ctx *primaryCtx) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *primaryCtx) Err() error {
	select {
	case <-ctx.primaryCh:
		return ErrLeaseExpired
	default:
		return ctx.parent.Err()
	}
}

func (ctx *primaryCtx) Value(key any) any {
	return ctx.parent.Value(key)
}

// removeFilesExcept removes all files from a directory except a given filename.
// Attempts to remove all files, even in the event of an error. Returns the
// first error encountered.
func removeFilesExcept(dir, filename string) (retErr error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, ent := range ents {
		// Skip directories & exception file.
		if ent.IsDir() || ent.Name() == filename {
			continue
		}
		if err := os.Remove(filepath.Join(dir, ent.Name())); retErr == nil {
			retErr = err
		}
	}

	return retErr
}

// sleepWithContext sleeps for a given amount of time or until the context is canceled.
func sleepWithContext(ctx context.Context, d time.Duration) {
	// Skip timer creation if context is already canceled.
	if ctx.Err() != nil {
		return
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Store metrics.
var (
	storeDBCountMetric = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "litefs_db_count",
		Help: "Number of managed databases.",
	})

	storeIsPrimaryMetric = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "litefs_is_primary",
		Help: "Primary status of the node.",
	})

	storeSubscriberCountMetric = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "litefs_subscriber_count",
		Help: "Number of connected subscribers",
	})
)
