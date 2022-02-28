package lib

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"github.com/cloudflare/circl/group"
	"github.com/decred/dcrd/lru"
	"github.com/dgraph-io/badger/v3"
	"github.com/golang/glog"
	"github.com/oleiade/lane"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// -------------------------------------------------------------------------------------
// StateChecksum
// -------------------------------------------------------------------------------------

// StateChecksum is used to verify integrity of state data. When syncing state from
// peers, we need to ensure that we are receiving the same copy of the database.
// Traditionally, this has been done using Merkle trees; however, Merkle trees incur
// O(log n) computational complexity for updates and O(n) space complexity, where n
// is the number of leaves in the tree.
//
// Instead, we use a structure that allows us to have an O(1) time and O(1) space complexity. We call
// our checksum construction EllipticSum as the checksum of the data is represented by a sum of
// elliptic curve points. To verify integrity of the data, we only need a single ec point.
// EllipticSum is inspired by the generalized k-sum problem, where given a set of uniformly random
// numbers L, we want to find a subset of k elements in L that XORs to 0. However, this XOR problem
// can be solved efficiently with dynamic programming in linear time.
//
// k-sum can be generalized to any algebraic group. That is, given a group G, identity element 0,
// operation +, and some set L of random group elements, find a subset (a_0, a_1, ..., a_k) such that
// a_0 + a_1 + ... + a_k = 0
// Turns out this problem is equivalent to the DLP in G and has a computational lower bound
// of O(sqrt(p)) where p is the smallest prime dividing the order of G.
// https://link.springer.com/content/pdf/10.1007%2F3-540-45708-9_19.pdf
//
// We use an elliptic curve group Ristretto255 and hash state data directly on the curve
// using the hash_to_curve primitive based on elligator2. The hash_to_curve mapping takes a
// byte array input and outputs a point via a mapping indistinguishable from a random function.
// Hash_to_curve does not reveal the discrete logarithm of the output points.
// According to the O(sqrt(p)) lower bound, Ristretto255 guarantees 126 bits of security.
type StateChecksum struct {
	// curve is the Ristretto255 elliptic curve group.
	curve group.Group
	// checksum is the ec point we use for verifying integrity of state data. It represents
	// the sum of points associated with all individual db records.
	checksum group.Element
	// dst string for the hash_to_curve function. This works like a seed.
	dst []byte

	// semaphore is used to manage parallelism when computing the state checksum. We use
	// a worker pool pattern of spawning a bounded number of threads to compute the checksum
	// in parallel. This allows us to compute it much more efficiently.
	semaphore *semaphore.Weighted
	// ctx is a helper variable used by the semaphore.
	ctx context.Context

	// When we want to add a database record to the state checksum, we will first have to
	// map the record to the Ristretto255 curve using the hash_to_curve. We will then add the
	// output point to the checksum. The hash_to_curve operation is about 2-3 orders of magnitude
	// slower than the point addition, therefore we will compute the hash_to_curve in parallel
	// and then add the output points to the checksum sequentially while holding a mutex.
	addMutex sync.Mutex

	// maxWorkers is the maximum number of workers we can have in the worker pool.
	maxWorkers int64
}

// Initialize starts the state checksum by initializing it to the identity element.
func (sc *StateChecksum) Initialize() {
	// Set the elliptic curve group to Ristretto255 and initialize checksum as identity.
	sc.curve = group.Ristretto255
	sc.checksum = sc.curve.Identity()
	// Set the dst string.
	sc.dst = []byte("DESO-ELLIPTIC-SUM:Ristretto255")

	// Set max workers to the number of available threads.
	sc.maxWorkers = int64(runtime.GOMAXPROCS(0))

	// Set the worker pool semaphore and context.
	sc.semaphore = semaphore.NewWeighted(sc.maxWorkers)
	sc.ctx = context.TODO()

}

// AddBytes adds record bytes to the checksum in parallel.
func (sc *StateChecksum) AddBytes(bytes []byte) error {
	// First check if we can add another worker to the worker pool by trying to increment the semaphore.
	if err := sc.semaphore.Acquire(sc.ctx, 1); err != nil {
		return errors.Wrapf(err, "StateChecksum.AddBytes: problem acquiring semaphore")
	}

	// Spawn a go routine that will decrement the semaphore and add the bytes to the checksum.
	go func(sc *StateChecksum, bytes []byte) {
		defer sc.semaphore.Release(1)

		// Compute the hash_to_curve primitive and map the bytes to an elliptic curve point.
		hashElement := sc.curve.HashToElement(bytes, sc.dst)

		// Hold the lock on addMutex to add the bytes to the checksum sequentially.
		sc.addMutex.Lock()
		sc.checksum.Add(sc.checksum, hashElement)
		sc.addMutex.Unlock()
	}(sc, bytes)

	return nil
}

// RemoveBytes works similarly to AddBytes.
func (sc *StateChecksum) RemoveBytes(bytes []byte) error {
	// First check if we can add another worker to the worker pool by trying to increment the semaphore.
	if err := sc.semaphore.Acquire(sc.ctx, 1); err != nil {
		return errors.Wrapf(err, " StateChecksum.RemoveBytes: problem acquiring semaphore")
	}

	// Spawn a go routine that will decrement the semaphore and remove the bytes from the checksum.
	go func(sc *StateChecksum, bytes []byte) {
		defer sc.semaphore.Release(1)

		// To remove bytes from the checksum, we will compute the inverse of the provided data
		// and add it to the checksum. Since the checksum is a sum of ec points, adding an inverse
		// of a previously added point will remove that point from the checksum. If we've previously
		// added point (x, y) to the checksum, we will be now adding the inverse (x, -y).
		hashElement := sc.curve.HashToElement(bytes, sc.dst)
		hashElement = hashElement.Neg(hashElement)

		// Hold the lock on addMutex to add the bytes to the checksum sequentially.
		sc.addMutex.Lock()
		sc.checksum.Add(sc.checksum, hashElement)
		sc.addMutex.Unlock()
	}(sc, bytes)
	return nil
}

// GetChecksum is used to get the checksum elliptic curve element.
func (sc *StateChecksum) GetChecksum() (group.Element, error) {
	// To get the checksum we will wait for all the current worker threads to finish.
	// To do so, we can just try to acquire sc.maxWorkers in the semaphore.
	if err := sc.semaphore.Acquire(sc.ctx, sc.maxWorkers); err != nil {
		return nil, errors.Wrapf(err, "StateChecksum.GetChecksum: problem acquiring semaphore")
	}
	defer sc.semaphore.Release(sc.maxWorkers)

	// Clone the checksum by adding it to identity. That's faster than doing ToBytes / FromBytes
	checksumCopy := group.Ristretto255.Identity()
	checksumCopy.Add(checksumCopy, sc.checksum)

	return checksumCopy, nil
}

func (sc *StateChecksum) Wait() error {
	if err := sc.semaphore.Acquire(sc.ctx, sc.maxWorkers); err != nil {
		return errors.Wrapf(err, "StateChecksum.Wait: problem acquiring semaphore")
	}
	defer sc.semaphore.Release(sc.maxWorkers)
	return nil
}

// ToBytes gets the checksum point encoded in compressed format as a 33 byte array.
// Note: Don't use this function to deep copy the checksum, use GetChecksum instead.
// ToBytes is doing an inverse square root, so it is slow.
func (sc *StateChecksum) ToBytes() ([]byte, error) {
	// Get the checksum.
	checksum, err := sc.GetChecksum()
	if err != nil {
		return nil, errors.Wrapf(err, "StateChecksum.ToBytes: problem getting checksum")
	}

	// Encode checksum to bytes.
	checksumBytes, err := checksum.MarshalBinary()
	if err != nil {
		return nil, errors.Wrapf(err, "stateChecksum.ToBytes: error during MarshalBinary")
	}
	return checksumBytes, nil
}

// -------------------------------------------------------------------------------------
// DBEntry
// -------------------------------------------------------------------------------------

// DBEntry is used to represent a database record. It's more convenient than passing
// <key, value> everywhere.
type DBEntry struct {
	Key   []byte
	Value []byte
}

func (entry *DBEntry) Encode() []byte {
	data := []byte{}

	data = append(data, UintToBuf(uint64(len(entry.Key)))...)
	data = append(data, entry.Key...)
	data = append(data, UintToBuf(uint64(len(entry.Value)))...)
	data = append(data, entry.Value...)
	return data
}

func (entry *DBEntry) Decode(rr io.Reader) error {
	var keyLen, entryLen uint64
	var err error

	// Decode key.
	keyLen, err = ReadUvarint(rr)
	if err != nil {
		return err
	}
	entry.Key = make([]byte, keyLen)
	_, err = io.ReadFull(rr, entry.Key)
	if err != nil {
		return err
	}

	// Decode value.
	entryLen, err = ReadUvarint(rr)
	if err != nil {
		return err
	}
	entry.Value = make([]byte, entryLen)
	_, err = io.ReadFull(rr, entry.Value)
	if err != nil {
		return err
	}

	return nil
}

// KeyValueToDBEntry is used to instantiate db entry from a <key, value> pair.
func KeyValueToDBEntry(key []byte, value []byte) *DBEntry {
	dbEntry := &DBEntry{}
	// Encode the key.
	dbEntry.Key = make([]byte, len(key))
	copy(dbEntry.Key, key)

	// Encode the value.
	dbEntry.Value = make([]byte, len(value))
	copy(dbEntry.Value, value)

	return dbEntry
}

// EmptyDBEntry indicates an empty DB entry. It's used for convenience.
func EmptyDBEntry() *DBEntry {
	// We do not use prefix 0 for state so we can use it in the empty DBEntry.
	return &DBEntry{
		Key:   []byte{0},
		Value: []byte{},
	}
}

// IsEmpty return true if the DBEntry is empty, false otherwise.
func (entry *DBEntry) IsEmpty() bool {
	return bytes.Equal(entry.Key, []byte{0})
}

// -------------------------------------------------------------------------------------
// Snapshot
// -------------------------------------------------------------------------------------

// List of database prefixes corresponding to the snapshot database.
var (
	// Prefix to store ancestral records. Ancestral records represent historical
	// values of main db entries that were modified during a snapshot epoch. For
	// instance if we modified some record <key, value> -> <key, value_new> in
	// the main db, we will store <key, value> in the ancestral records under:
	//  <prefix [1]byte, blockheight [8]byte, key []byte> -> <value []byte, existence_byte [1]byte>
	// The existence_byte is either 0 or 1 depending on whether <key, value>
	// previously existed in the main db. The existence_byte allows us to quickly
	// determine if a given record existed in a snapshot without making additional
	// lookups. It is part of the value, instead of the key, so that we can confirm
	// that a given key has already been saved to the ancestral records by performing
	// just a single lookup. If it was part of the key, we would have needed two.
	_prefixAncestralRecord = []byte{0}

	// Snapshot health is used to indicate if the snapshot was properly saved in
	// the database. In case something went wrong with the snapshot and we rebooted
	// the node, we would need to save health information about the snapshot in the db.
	//  <prefix> -> <health byte [1]byte>
	_prefixSnapshotHealth = []byte{1}
)

// AncestralCache is an in-memory structure that helps manage concurrency between node's
// main db flushes and ancestral records flushes. For each main db flush transaction, we
// will build an ancestral cache that contains maps of historical values that were in the
// main db before we flushed. In particular, we distinguish between existing and
// non-existing records. Existing records are those records that had already been present
// in the main db prior to the flush. Non-existing records were not present in the main db,
// and the flush added them for the first time.
//
// The AncestralCache is stored in the Snapshot struct in a concurrency-safe deque (bi-directional
// queue). This deque follows a pub-sub pattern where the main db thread pushes ancestral
// caches onto the deque. The snapshot thread then consumes these objects and writes to the
// ancestral records. We decided for this pattern because it doesn't slow down the main
// block processing thread. However, to make this fully work, we also need some bookkeeping
// to ensure the ancestral record flushes are up-to-date with the main db flushes. We
// solve this with non-blocking counters (MainDBSemaphore, AncestralDBSemaphore) that count
// the total number of flushes to main db and ancestral records.
type AncestralCache struct {
	// id is used to identify the AncestralCache
	id uint64

	// ExistingRecordsMap keeps track of original main db of records that we modified during
	// utxo_view flush, which is where we're modifying state data.
	ExistingRecordsMap map[string][]byte

	// NonExistingRecordsMap keeps track of records that didn't exist prior to utxo_view flush.
	NonExistingRecordsMap map[string]bool

	// RecordsKeyList is a list of keys in ExistingRecordsMap and NonExistingRecordsMap. We will sort
	// it so that LSM tree lookups in BadgerDB are faster
	RecordsKeyList []string
}

func NewAncestralCache(id uint64) *AncestralCache {
	return &AncestralCache{
		id:                    id,
		ExistingRecordsMap:    make(map[string][]byte),
		NonExistingRecordsMap: make(map[string]bool),
		RecordsKeyList:        make([]string, 0),
	}
}

// SnapshotOperationType define the different operations that can be enqueued to the snapshot's OperationChannel.
type SnapshotOperationType uint8

const (
	// SnapshotOperationFlush operation enqueues a flush to the ancestral records.
	SnapshotOperationFlush SnapshotOperationType = iota
	// SnapshotOperationProcessBlock operation signals that a new block has been added to the blockchain.
	SnapshotOperationProcessBlock
	// SnapshotOperationProcessChunk operation is enqueued when we receive a snapshot chunk during syncing.
	SnapshotOperationProcessChunk
	// SnapshotOperationChecksumAdd operation is enqueued when we want to add bytes to the state checksum.
	SnapshotOperationChecksumAdd
	// SnapshotOperationChecksumRemove operation is enqueued when we want to remove bytes to the state checksum.
	SnapshotOperationChecksumRemove
	// SnapshotOperationChecksumPrint is called when we want to print the state checksum.
	SnapshotOperationChecksumPrint
)

// SnapshotOperation is passed in the snapshot's OperationChannel.
type SnapshotOperation struct {
	// operationType determines the operation.
	operationType SnapshotOperationType

	/* SnapshotOperationProcessBlock */
	// blockNode is the processed block.
	blockNode *BlockNode

	/* SnapshotOperationProcessChunk */
	// mainDb is the main db instance.
	mainDb *badger.DB
	// snapshotChunk is the snapshot chunk received from the peer.
	snapshotChunk []*DBEntry

	/* SnapshotOperationChecksumAdd, SnapshotOperationChecksumRemove */
	// checksumBytes is the bytes we want to add to the checksum, passed during
	checksumBytes []byte

	/* SnapshotOperationChecksumPrint */
	// printText is the text we want to put in the print statement.
	printText string
}

// Snapshot is the main data structure used in hyper sync. It manages the creation of the database
// snapshot (as the name suggests), which is a periodic copy of the node's state at certain block
// heights, separated by a constant period. This period is defined by SnapshotBlockHeightPeriod,
// meaning Snapshot will build copies of the db at heights: 0, period, 2 * period, 3 * period, ...
// The blocks between the snapshot heights are referred to as a snapshot epoch.
//
// Cloning the database for infinite-state blockchains like DeSo would be extremely costly,
// incurring minutes of downtime. Instead, we use a structure called ancestral records, which
// is constructed on-the-go and only stores records modified during a snapshot epoch. This allows
// us to reconstruct the database at the last snapshot height by combining the ancestral record
// entries with the main db entries. This process has a significantly smaller computational and
// storage overhead. The ancestral records are stored in a separate database, that's modified
// asynchronously to the main db. This means that the main node thread is minimally affected by
// the snapshot computation. It also means that we need to manage the concurrency between these two
// databases. We will achieve this without locking through Snapshot's OperationChannel, to which
// the main thread will enqueue asynchronous operations such as ancestral records updates, checksum
// computation, snapshot operations, etc. In addition, Snapshot is used to serve state chunks
// to nodes that are booting using hyper sync. In such case, the Snapshot will fetch a portion
// of the snapshot database by scanning a section of the main db as well as relevant ancestral
// records, to combine them into a chunk representing the database at past snapshot height.
// Summarizing, Snapshot serves three main purposes:
// 	- maintaining ancestral records
// 	- managing the state checksum
// 	- serving snapshot chunks to syncing nodes.
type Snapshot struct {
	// AncestralRecordsDb is used to store ancestral records
	AncestralRecordsDb *badger.DB
	// AncestralMemory stores information about the ancestral records that should be flushed into the db.
	// We use a concurrency-safe deque which allows us to push objects to the end of the AncestralMemory
	// queue in one thread and consume objects from the beginning of the queue in another thread without
	// concurrency issues. The objects that we push to the queue will be instances of AncestralCache.
	AncestralMemory *lane.Deque

	// DatabaseCache is used to store most recent DB records that we've read/wrote.
	// This is particularly useful for maintaining ancestral records, because
	// it saves us read time when we're writing to DB during utxo_view flush.
	DatabaseCache lru.KVCache

	// SnapshotBlockHeight is the height of the last snapshot.
	SnapshotBlockHeight uint64

	// AncestralFlushCounter is used to offset ancestral records flush to occur only after x blocks.
	AncestralFlushCounter uint64

	// SnapshotBlockHeightPeriod is the constant height offset between individual snapshot epochs.
	SnapshotBlockHeightPeriod uint64

	// OperationChannel is used to enqueue actions to the main snapshot Run loop. It is used to
	// schedule actions such as ancestral records updates, checksum computation, snapshot operations.
	OperationChannel chan *SnapshotOperation

	// Checksum allows us to confirm integrity of the state so that when we're syncing with peers,
	// we are confident that data wasn't tampered with.
	Checksum *StateChecksum
	// CurrentEpochChecksumBytes is the bytes of the state checksum for the snapshot at the current epoch.
	CurrentEpochChecksumBytes []byte
	// CurrentEpochBlockHash is the hash of the first block of the current epoch. It's used to identify the snapshot.
	CurrentEpochBlockHash *BlockHash

	// MainDBSemaphore and AncestralDBSemaphore are atomically accessed counter semaphores that will be
	// used to control race conditions between main db and ancestral records.
	MainDBSemaphore      int32
	AncestralDBSemaphore int32
	// SemaphoreLock is held whenever we modify the MainDBSemaphore or AncestralDBSemaphore.
	SemaphoreLock sync.Mutex

	// brokenSnapshot indicates that we need to rebuild entire snapshot from scratch.
	// Updates to the snapshot happen in the background, so sometimes they can be broken
	// if node stops unexpectedly. Health checks will detect these and set brokenSnapshot.
	brokenSnapshot bool

	// ExitChannel is used to stop the snapshot when shutting down the node.
	ExitChannel chan bool
	// updateWaitGroup is used to wait for snapshot loop to finish.
	updateWaitGroup sync.WaitGroup

	timer *Timer
}

// NewSnapshot creates a new snapshot instance.
func NewSnapshot(dataDirectory string, snapshotBlockHeightPeriod uint64) (*Snapshot, error) {
	// TODO: make sure we don't snapshot when using PG
	// Initialize the ancestral records database
	snapshotDir := filepath.Join(GetBadgerDbPath(dataDirectory), "snapshot")
	snapshotOpts := badger.DefaultOptions(snapshotDir)
	snapshotOpts.ValueDir = GetBadgerDbPath(snapshotDir)
	snapshotOpts.MemTableSize = 1024 << 20
	snapshotDb, err := badger.Open(snapshotOpts)
	if err != nil {
		glog.Fatal(err)
	}
	glog.Infof("Snapshot BadgerDB Dir: %v", snapshotOpts.Dir)
	glog.Infof("Snapshot BadgerDB ValueDir: %v", snapshotOpts.ValueDir)
	if snapshotBlockHeightPeriod == 0 {
		snapshotBlockHeightPeriod = SnapshotBlockHeightPeriod
	}

	// Initialize the checksum.
	checksum := &StateChecksum{}
	checksum.Initialize()

	// Initialize the timer.
	timer := &Timer{}
	timer.Initialize()

	// Set the snapshot.
	snap := &Snapshot{
		AncestralRecordsDb:        snapshotDb,
		DatabaseCache:             lru.NewKVCache(DatabaseCacheSize),
		SnapshotBlockHeight:       uint64(0),
		AncestralFlushCounter:     uint64(0),
		SnapshotBlockHeightPeriod: snapshotBlockHeightPeriod,
		OperationChannel:          make(chan *SnapshotOperation, 10000),
		Checksum:                  checksum,
		CurrentEpochChecksumBytes: []byte{},
		AncestralMemory:           lane.NewDeque(),
		MainDBSemaphore:           int32(0),
		AncestralDBSemaphore:      int32(0),
		brokenSnapshot:            false,
		timer:                     timer,
		ExitChannel:               make(chan bool),
	}
	// Run the snapshot main loop.
	go snap.Run()

	return snap, nil
}

// Run is the snapshot main loop. It handles the operations from the OperationChannel.
func (snap *Snapshot) Run() {
	glog.Infof("Snapshot.Run: Starting update thread")

	snap.updateWaitGroup.Add(1)
	for {
		select {
		case <-snap.ExitChannel:
			snap.updateWaitGroup.Done()
			return
		case operation := <-snap.OperationChannel:
			switch operation.operationType {
			case SnapshotOperationFlush:
				glog.Infof("Snapshot.Run: Flushing ancestral records with counter")
				snap.FlushAncestralRecords()

			case SnapshotOperationProcessBlock:
				height := uint64(operation.blockNode.Height)
				glog.Infof("Snapshot.Run: Getting into the delete channel with height (%v)", height)
				if height%snap.SnapshotBlockHeightPeriod == 0 {
					var err error
					glog.Infof("Snapshot.Run: About to delete SnapshotBlockHeight (%v) and set new height (%v)",
						snap.SnapshotBlockHeight, height)
					snap.SnapshotBlockHeight = height
					snap.CurrentEpochChecksumBytes, err = snap.Checksum.ToBytes()
					if err != nil {
						glog.Errorf("Snapshot.Run: Problem getting checksum bytes (%v)", err)
					}
					snap.CurrentEpochBlockHash = operation.blockNode.Hash
					glog.Infof("Snapshot.Run: snapshot is (%v)", snap.CurrentEpochChecksumBytes)
					// TODO: This should remove past height not current height.
					snap.DeleteAncestralRecords(height)
				}

			case SnapshotOperationProcessChunk:
				if err := snap.SetSnapshotChunk(operation.mainDb, operation.snapshotChunk); err != nil {
					glog.Errorf("Snapshot.Run: Problem adding snapshot chunk to the db")
				}

			case SnapshotOperationChecksumAdd:
				if err := snap.Checksum.AddBytes(operation.checksumBytes); err != nil {
					glog.Errorf("Snapshot.Run: Problem adding checksum bytes operation (%v)", operation)
				}

			case SnapshotOperationChecksumRemove:
				if err := snap.Checksum.RemoveBytes(operation.checksumBytes); err != nil {
					glog.Errorf("Snapshot.Run: Problem removing checksum bytes operation (%v)", operation)
				}

			case SnapshotOperationChecksumPrint:
				stateChecksum, err := snap.Checksum.ToBytes()
				if err != nil {
					glog.Errorf("Snapshot.ChecksumPrint: Problem getting checksum bytes (%v)", err)
				}
				glog.Infof("Snapshot.ChecksumPrint: PrintText (%s) Current checksum (%v)", operation.printText, stateChecksum)
			}
		}
	}
}

func (snap *Snapshot) Stop() {
	glog.Infof("Snapshot.Stop: Stopping the run loop")

	snap.ExitChannel <- true
	snap.updateWaitGroup.Wait()

	snap.AncestralRecordsDb.Close()
}

// EnqueueAncestralRecordsFlush updates the ancestral records after a utxo_view flush.
// This function should be called in a go-routine after all utxo_view flushes.
func (snap *Snapshot) EnqueueAncestralRecordsFlush() {
	// If snapshot is broken then there's nothing to do.
	glog.Infof("Snapshot.EnqueueAncestralRecordsFlush: Initiated the flush")

	if snap.brokenSnapshot {
		glog.Errorf("Snapshot.EnqueueAncestralRecordsFlush: Broken snapshot, aborting")
		return
	}

	// Signal that the main db update has finished by incrementing the main semaphore.
	// Also signal that the ancestral db write started by increasing the ancestral semaphore.
	snap.SemaphoreLock.Lock()
	snap.MainDBSemaphore += 1
	snap.AncestralDBSemaphore += 1
	snap.SemaphoreLock.Unlock()
	glog.Infof("Snapshot.EnqueueAncestralRecordsFlush: Sending counter (%v) to the CounterChannel", snap.AncestralFlushCounter)
	// We send the flush counter to the counter to indicate that a flush should take place.
	snap.OperationChannel <- &SnapshotOperation{
		operationType: SnapshotOperationFlush,
	}
	return
}

func (snap *Snapshot) PrintChecksum(text string) {
	snap.OperationChannel <- &SnapshotOperation{
		operationType: SnapshotOperationChecksumPrint,
		printText:     text,
	}
}

func (snap *Snapshot) FinishProcessBlock(blockNode *BlockNode) {
	snap.OperationChannel <- &SnapshotOperation{
		operationType: SnapshotOperationProcessBlock,
		blockNode:     blockNode,
	}
}

func (snap *Snapshot) ProcessSnapshotChunk(mainDb *badger.DB, snapshotChunk []*DBEntry) {
	snap.OperationChannel <- &SnapshotOperation{
		operationType: SnapshotOperationProcessChunk,
		mainDb:        mainDb,
		snapshotChunk: snapshotChunk,
	}
}

func (snap *Snapshot) AddChecksumBytes(bytes []byte) {
	snap.OperationChannel <- &SnapshotOperation{
		operationType: SnapshotOperationChecksumAdd,
		checksumBytes: bytes,
	}
}

func (snap *Snapshot) RemoveChecksumBytes(bytes []byte) {
	snap.OperationChannel <- &SnapshotOperation{
		operationType: SnapshotOperationChecksumRemove,
		checksumBytes: bytes,
	}
}

// WaitForAllOperationsToFinish will busy-wait for the snapshot channel to process all
// current operations. Spinlocks are undesired but it's the easiest solution in this case,
func (snap *Snapshot) WaitForAllOperationsToFinish() {
	for {
		if len(snap.OperationChannel) > 0 {
			continue
		}
		break
	}
}

// PrepareAncestralRecordsFlush adds a new instance of ancestral cache to the AncestralMemory deque.
// It must be called prior to calling EnqueueAncestralRecordsFlush.
func (snap *Snapshot) PrepareAncestralRecordsFlush() {
	// Signal that the main db update has started by incrementing the main semaphore.
	snap.SemaphoreLock.Lock()
	snap.MainDBSemaphore += 1
	snap.SemaphoreLock.Unlock()

	snap.AncestralFlushCounter += 1
	index := snap.AncestralFlushCounter

	snap.AncestralMemory.Append(NewAncestralCache(index))

	glog.Infof("Snapshot.PrepareAncestralRecordsFlush: Created structs at index (%v)", index)
}

// PrepareAncestralRecord prepares an individual ancestral record in the last ancestral cache.
// It will add the record to ExistingRecordsMap or NonExistingRecordsMap adequately.
func (snap *Snapshot) PrepareAncestralRecord(key string, value []byte, existed bool) error {
	// If the record was not found, we add it to the NonExistingRecordsMap, otherwise to ExistingRecordsMap.
	// We record the key in RecordsKeyList.
	index := snap.AncestralFlushCounter

	if snap.AncestralMemory.Empty() {
		return fmt.Errorf("Snapshot.PrepareAncestralRecord: ancestral memory is empty. " +
			"Did you forget to call Snapshot.PrepareAncestralRecordsFlush?")
	}

	// Get the last ancestral cache. This is where we'll add the new record to.
	lastAncestralCache := snap.AncestralMemory.Last().(*AncestralCache)
	if lastAncestralCache.id != index {
		return fmt.Errorf("Snapshot.PrepareAncestralRecords: last ancestral cache index (%v) is "+
			"greater than current flush index (%v)", lastAncestralCache.id, index)
	}

	// If the record already exists in the ancestral cache ExistingRecordsMap, skip.
	if _, ok := lastAncestralCache.ExistingRecordsMap[key]; ok {
		return nil
	}

	// If the record already exists in the ancestral cache NonExistingRecordsMap, skip.
	if _, ok := lastAncestralCache.NonExistingRecordsMap[key]; ok {
		return nil
	}

	// Add the record to the records key list and the adequate records list.
	lastAncestralCache.RecordsKeyList = append(lastAncestralCache.RecordsKeyList, key)
	if existed {
		lastAncestralCache.ExistingRecordsMap[key] = value
	} else {
		lastAncestralCache.NonExistingRecordsMap[key] = true
	}
	return nil
}

// GetAncestralRecordsKey is used to get an ancestral record key from a main DB key.
// 	<prefix [1]byte, block height [8]byte, key []byte> -> <value []byte, existence_byte [1]byte>
func (snap *Snapshot) GetAncestralRecordsKey(key []byte) []byte {
	var prefix []byte

	// Append the ancestral records prefix.
	prefix = append(prefix, _prefixAncestralRecord...)

	// Append block height, which is the current snapshot identifier.
	prefix = append(prefix, EncodeUint64(snap.SnapshotBlockHeight)...)

	// Finally, append the main DB key.
	prefix = append(prefix, key...)
	return prefix
}

// AncestralRecordToDBEntry is used to translate the <ancestral_key, ancestral_value> pairs into
// the actual <key, value> pairs. Ancestral records have the format:
// 	<prefix [1]byte, block height [8]byte, key []byte> -> <value []byte, existence_byte [1]byte>
// So we need to trim the first 9 bytes off of the ancestral_key to get the actual key.
// And we need to trim the last 1 byte off of the ancestral_value to get the actual value.
func (snap *Snapshot) AncestralRecordToDBEntry(ancestralEntry *DBEntry) *DBEntry {
	var dbKey, dbVal []byte
	// Trim the prefix and the block height from the ancestral record key.
	if len(ancestralEntry.Key) > 9 {
		dbKey = ancestralEntry.Key[9:]
	} else {
		dbKey = ancestralEntry.Key
	}

	// Trim the existence_byte from the ancestral record value.
	if len(ancestralEntry.Value) > 0 {
		dbVal = ancestralEntry.Value[:len(ancestralEntry.Value)-1]
	}
	return &DBEntry{
		Key:   dbKey,
		Value: dbVal,
	}
}

// CheckAnceststralRecordExistenceByte checks the existence_byte in the ancestral record value.
func (snap *Snapshot) CheckAnceststralRecordExistenceByte(value []byte) bool {
	if len(value) > 0 {
		return value[len(value)-1] == 1
	}
	return false
}

// isState determines if a key is a state-related record.
func (snap *Snapshot) isState(key []byte) bool {
	return isStateKey(key) && !snap.brokenSnapshot
}

// isFlushing checks whether a main DB flush or ancestral record flush is taking place.
func (snap *Snapshot) isFlushing() bool {
	// We retrieve the ancestral record and main db semaphores.
	snap.SemaphoreLock.Lock()
	ancestralDBSemaphore := snap.AncestralDBSemaphore
	mainDBSemaphore := snap.MainDBSemaphore
	snap.SemaphoreLock.Unlock()

	// Flush is taking place if the semaphores have different counters or if they are odd.
	// We increment each semaphore whenever we start the flush and when we end it so they are always
	// even when the DB is not being updated.
	if ancestralDBSemaphore != mainDBSemaphore ||
		(ancestralDBSemaphore|mainDBSemaphore)%2 == 1 {

		return true
	}
	return false
}

// FlushAncestralRecords updates the ancestral records after a utxo_view flush.
// This function should be called in a go-routine after all utxo_view flushes.
func (snap *Snapshot) FlushAncestralRecords() {
	glog.Infof("Snapshot.EnqueueAncestralRecordsFlush: Initiated the flush")

	// If snapshot is broken then there's nothing to do.
	if snap.brokenSnapshot {
		glog.Errorf("Snapshot.EnqueueAncestralRecordsFlush: Broken snapshot, aborting")
		return
	}

	// Make sure we've finished all checksum computation before we proceed with the flush.
	// Since this gets called after all snapshot operations is enqueued after the main db
	// flush, the order of operations is preserved; however, there could still be some
	// snapshot worker threads running so we want to wait until they're done.
	err := snap.Checksum.Wait()
	if err != nil {
		glog.Errorf("Snapshot.EnqueueAncestralRecordsFlush: Error while waiting for checksum: (%v)", err)
		return
	}

	lastAncestralCache := snap.AncestralMemory.First().(*AncestralCache)
	// First sort the copyMapKeyList so that we write to BadgerDB in order.
	sort.Strings(lastAncestralCache.RecordsKeyList)
	glog.V(2).Infof("Snapshot.EnqueueAncestralRecordsFlush: Finished sorting map keys")

	// We launch a new read-write transaction to set the records.

	err = snap.AncestralRecordsDb.Update(func(txn *badger.Txn) error {
		// In case we kill the node in the middle of this update.
		err := txn.Set(_prefixSnapshotHealth, []byte{0})
		if err != nil {
			return errors.Wrapf(err, "Snapshot.EnqueueAncestralRecordsFlush: Problem flushing "+
				"snapshot health")
		}
		// Iterate through all now-sorted keys.
		glog.V(2).Infof("Snapshot.EnqueueAncestralRecordsFlush: Adding (%v) new records", len(lastAncestralCache.RecordsKeyList))
		glog.V(2).Infof("Snapshot.EnqueueAncestralRecordsFlush: Adding (%v) ExistingRecordsMap records", len(lastAncestralCache.ExistingRecordsMap))
		glog.V(2).Infof("Snapshot.EnqueueAncestralRecordsFlush: Adding (%v) NonExistingRecordsMap records", len(lastAncestralCache.NonExistingRecordsMap))
		for _, key := range lastAncestralCache.RecordsKeyList {
			// We store keys as strings because they're easier to store and sort this way.
			keyBytes, err := hex.DecodeString(key)
			if err != nil {
				return errors.Wrapf(err, "Snapshot.EnqueueAncestralRecordsFlush: Problem "+
					"decoding copyMapKeyList key: %v", key)
			}

			// We check whether this record is already present in ancestral records,
			// if so then there's nothing to do. What we want is err == badger.ErrKeyNotFound
			_, err = txn.Get(snap.GetAncestralRecordsKey(keyBytes))
			if err != nil && err != badger.ErrKeyNotFound {
				return errors.Wrapf(err, "Snapshot.EnqueueAncestralRecordsFlush: Problem "+
					"reading exsiting record in the DB at key: %v", key)
			}
			if err == nil {
				continue
			}

			// If we get here, it means that no record existed in ancestral records at key.
			// The key was either added to copyAncestralMap or copyNotExistsMap during flush.
			if value, exists := lastAncestralCache.ExistingRecordsMap[key]; exists {
				err = txn.Set(snap.GetAncestralRecordsKey(keyBytes), append(value, byte(1)))
				if err != nil {
					return errors.Wrapf(err, "Snapshot.EnqueueAncestralRecordsFlush: Problem "+
						"flushing a record from copyAncestralMap at key %v:", key)
				}
			} else if _, exists = lastAncestralCache.NonExistingRecordsMap[key]; exists {
				err = txn.Set(snap.GetAncestralRecordsKey(keyBytes), []byte{byte(0)})
				if err != nil {
					return errors.Wrapf(err, "Snapshot.EnqueueAncestralRecordsFlush: Problem "+
						"flushing a record from copyNotExistsMap at key %v:", key)
				}
			} else {
				return fmt.Errorf("Snapshot.EnqueueAncestralRecordsFlush: Error, key is not " +
					"in copyAncestralMap nor copyNotExistsMap. This should never happen")
			}
		}
		err = txn.Set(_prefixSnapshotHealth, []byte{1})
		if err != nil {
			return errors.Wrapf(err, "Snapshot.EnqueueAncestralRecordsFlush: Problem flushing "+
				"snapshot health")
		}
		return nil
	})
	if err != nil {
		// If any error occurred, then the snapshot is potentially broken.
		snap.brokenSnapshot = true
		glog.Errorf("Snapshot.EnqueueAncestralRecordsFlush: Problem flushing snapshot %v, Error %v", snap, err)
	}

	// Signal that the ancestral db write has finished by incrementing the semaphore.
	snap.SemaphoreLock.Lock()
	snap.AncestralDBSemaphore += 1
	snap.SemaphoreLock.Unlock()
	snap.AncestralMemory.Shift()
	glog.Infof("Snapshot.EnqueueAncestralRecordsFlush: finished flushing ancestral records. Snapshot "+
		"status, brokenSnapshot: (%v)", snap.brokenSnapshot)
}

// DeleteAncestralRecords is used to delete ancestral records for the provided height.
func (snap *Snapshot) DeleteAncestralRecords(height uint64) {

	var prefix []byte
	prefix = append(prefix, EncodeUint64(height)...)

	glog.Infof("Snapshot.DeleteAncestralRecords: Starting delete process for height (%v)", height)
	numDeleted := 0
	err := snap.AncestralRecordsDb.DropPrefix(prefix)
	if err != nil {
		glog.Errorf("Snapshot.DeleteAncestralRecords: Problem deleting ancestral records error (%v)", err)
		return
	}
	glog.Infof("Snapshot.DeleteAncestralRecords: Finished deleting for height (%v) total (%v)", height, numDeleted)
}

func (snap *Snapshot) String() string {
	return fmt.Sprintf("< Snapshot | height: %v | broken: %v >", snap.SnapshotBlockHeight, snap.brokenSnapshot)
}

// GetSnapshotChunk fetches a batch of records from the nodes DB that match the provided prefix and
// have a key at least equal to the startKey lexicographically. The function will also fetch ancestral
// records and combine them with the DB records so that the batch reflects an ancestral block.
func (snap *Snapshot) GetSnapshotChunk(mainDb *badger.DB, prefix []byte, startKey []byte) (
	_snapshotEntriesBatch []*DBEntry, _snapshotEntriesFilled bool, _concurrencyFault bool, _err error) {

	// Check if we're flushing to the main db or to the ancestral records. If a flush is currently
	// taking place, we will return a concurrencyFault error because the records are getting modified.
	snap.SemaphoreLock.Lock()
	ancestralDBSemaphoreBefore := snap.AncestralDBSemaphore
	mainDBSemaphoreBefore := snap.MainDBSemaphore
	snap.SemaphoreLock.Unlock()
	if snap.isFlushing() {
		return nil, false, true, nil
	}

	// This the list of fetched DB entries.
	var snapshotEntriesBatch []*DBEntry

	// Fetch the batch from main DB records with a batch size of about snap.BatchSize.
	mainDbBatchEntries, mainDbFilled, err := DBIteratePrefixKeys(mainDb, prefix, startKey, SnapshotBatchSize)
	if err != nil {
		return nil, false, false, errors.Wrapf(err, "Snapshot.GetSnapshotChunk: Problem fetching main Db records: ")
	}
	// Fetch the batch from the ancestral DB records with a batch size of about snap.BatchSize.
	ancestralDbBatchEntries, ancestralDbFilled, err := DBIteratePrefixKeys(snap.AncestralRecordsDb,
		snap.GetAncestralRecordsKey(prefix), snap.GetAncestralRecordsKey(startKey), SnapshotBatchSize)
	if err != nil {
		return nil, false, false, errors.Wrapf(err, "Snapshot.GetSnapshotChunk: Problem fetching main Db records: ")
	}

	// To combine the main DB entries and the ancestral records DB entries, we iterate through the ancestral records and
	// for each key we add all the main DB keys that are smaller than the currently processed key. The ancestral records
	// entries have priority over the main DB entries, so whenever there are entries with the same key among the two DBs,
	// we will only add the ancestral record entry to our snapshot batch. Also, the loop below might appear like O(n^2)
	// but it's actually O(n) because the inside loop iterates at most O(n) times in total.

	// Index to keep track of how many main DB entries we've already processed.
	indexChunk := 0
	for _, ancestralEntry := range ancestralDbBatchEntries {
		dbEntry := snap.AncestralRecordToDBEntry(ancestralEntry)

		for jj := indexChunk; jj < len(mainDbBatchEntries); {
			if bytes.Compare(mainDbBatchEntries[jj].Key, dbEntry.Key) == -1 {
				snapshotEntriesBatch = append(snapshotEntriesBatch, mainDbBatchEntries[jj])
			} else if bytes.Compare(mainDbBatchEntries[jj].Key, dbEntry.Key) == 1 {
				break
			}
			// if keys are equal we just skip
			jj++
			indexChunk = jj
		}
		if snap.CheckAnceststralRecordExistenceByte(ancestralEntry.Value) {
			snapshotEntriesBatch = append(snapshotEntriesBatch, dbEntry)
		}
		// If we filled the chunk for main db records, we will return so that there is no
		// gap between the most recently added DBEntry and the next ancestral record. Otherwise,
		// we will keep going with the loop and add all the ancestral records.
		if mainDbFilled && indexChunk == len(mainDbBatchEntries) {
			break
		}
	}

	// If we got all ancestral records, but there are still some main DB entries that we can add,
	// we will do that now.
	if !ancestralDbFilled {
		for jj := indexChunk; jj < len(mainDbBatchEntries); jj++ {
			indexChunk = jj
			snapshotEntriesBatch = append(snapshotEntriesBatch, mainDbBatchEntries[jj])
		}
	}

	// If no records are present in the db for the provided prefix and startKey, return an empty db entry.
	if len(snapshotEntriesBatch) == 0 {
		snapshotEntriesBatch = append(snapshotEntriesBatch, EmptyDBEntry())
		return snapshotEntriesBatch, false, false, nil
	}

	// Check if the semaphores have changed as we were fetching the snapshot chunk. It could happen
	// that a flush was taking place right when we were reading records from the database. To detect
	// such edge-case, we compare the current semaphore counters with the ones we've copied when
	// we started retrieving the database chunk.
	snap.SemaphoreLock.Lock()
	ancestralDBSemaphoreAfter := snap.AncestralDBSemaphore
	mainDBSemaphoreAfter := snap.MainDBSemaphore
	snap.SemaphoreLock.Unlock()
	if ancestralDBSemaphoreBefore != ancestralDBSemaphoreAfter ||
		mainDBSemaphoreBefore != mainDBSemaphoreAfter {
		return nil, false, true, nil
	}

	// If either of the chunks is full, we should return true.
	return snapshotEntriesBatch, mainDbFilled || ancestralDbFilled, false, nil
}

// SetSnapshotChunk is called to put the snapshot chunk that we've got from a peer in the database.
func (snap *Snapshot) SetSnapshotChunk(mainDb *badger.DB, chunk []*DBEntry) error {
	snap.timer.Start("SetSnapshotChunk.Total")
	// We use badgerDb write batches as it's the fastest way to write multiple records to the db.
	wb := mainDb.NewWriteBatch()
	defer wb.Cancel()
	for _, dbEntry := range chunk {
		snap.timer.Start("SetSnapshotChunk.Set")
		err := wb.Set(dbEntry.Key, dbEntry.Value) // Will create txns as needed.
		if err != nil {
			return err
		}
		snap.timer.End("SetSnapshotChunk.Set")

		// TODO: do this concurrently to the DB write and just wait at the end.
		snap.timer.Start("SetSnapshotChunk.Checksum")
		if err := snap.Checksum.AddBytes(EncodeKeyValue(dbEntry.Key, dbEntry.Value)); err != nil {
			glog.Errorf("Snapshot.SetSnapshotChunk: Problem adding checksum")
		}
		snap.timer.End("SetSnapshotChunk.Checksum")
	}

	snap.timer.Start("SetSnapshotChunk.Set")
	err := wb.Flush()
	if err != nil {
		return err
	}
	snap.timer.End("SetSnapshotChunk.Set")
	snap.timer.End("SetSnapshotChunk.Total")

	snap.timer.Print("SetSnapshotChunk.Total")
	snap.timer.Print("SetSnapshotChunk.Set")
	snap.timer.Print("SetSnapshotChunk.Checksum")
	return nil
}

// -------------------------------------------------------------------------------------
// Timer
// -------------------------------------------------------------------------------------

// Timer is used for convenience to time certain events during development.
type Timer struct {
	totalElapsedTimes map[string]float64
	lastTimes         map[string]time.Time
	productionMode    bool
}

func (t *Timer) Initialize() {
	t.totalElapsedTimes = make(map[string]float64)
	t.lastTimes = make(map[string]time.Time)
	// Change this to true to stop timing
	t.productionMode = false
}

func (t *Timer) Start(eventName string) {
	if t.productionMode {
		return
	}
	if _, exists := t.lastTimes[eventName]; !exists {
		t.totalElapsedTimes[eventName] = 0.0
	}
	t.lastTimes[eventName] = time.Now()
}

func (t *Timer) End(eventName string) {
	if t.productionMode {
		return
	}
	if _, exists := t.totalElapsedTimes[eventName]; !exists {
		glog.Errorf("Timer.End: Error called with non-existent eventName")
		return
	}
	t.totalElapsedTimes[eventName] += time.Since(t.lastTimes[eventName]).Seconds()
}

func (t *Timer) Print(eventName string) {
	if t.productionMode {
		return
	}
	if _, exists := t.lastTimes[eventName]; exists {
		glog.Infof("Timer.End: event (%s) total elapsed time (%v)",
			eventName, t.totalElapsedTimes[eventName])
	}
}