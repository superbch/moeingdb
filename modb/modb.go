package modb

import (
	"encoding/binary"
	"sync"

	"github.com/cespare/xxhash"
	"github.com/moeing-chain/MoeingADS/datatree"
	"github.com/moeing-chain/MoeingADS/indextree"

	"github.com/moeing-chain/MoeingDB/indexer"
	"github.com/moeing-chain/MoeingDB/types"
)

/*  Following keys are saved in rocksdb:
	"HPF_SIZE" the size of hpfile
	"SEED" seed for xxhash, used to generate short hash
	"NEW" new block's information for indexing, deleted after consumption
	"BXXXX" ('B' followed by 4 bytes) the indexing information for a block
*/

type RocksDB = indextree.RocksDB
type HPFile = datatree.HPFile

type MoDB struct {
	wg      sync.WaitGroup
	mtx     sync.RWMutex
	path    string
	metadb  *RocksDB
	hpfile  *HPFile
	blkBuf  []byte
	idxBuf  []byte
	seed    [8]byte
	indexer indexer.Indexer
}

var _ types.DB = (*MoDB)(nil)

func CreateEmptyMoDB(path string, seed [8]byte) *MoDB {
	metadb, err := indextree.NewRocksDB("rocksdb", path)
	if err != nil {
		panic(err)
	}
	hpfile, err := datatree.NewHPFile(8*1024*1024, 2048*1024*1024, path+"/data")
	if err != nil {
		panic(err)
	}
	db := &MoDB{
		path:    path,
		metadb:  metadb,
		hpfile:  &hpfile,
		blkBuf:  make([]byte, 0, 1024),
		idxBuf:  make([]byte, 0, 1024),
		seed:    seed,
		indexer: indexer.New(),
	}
	var zero [8]byte
	db.metadb.OpenNewBatch()
	db.metadb.CurrBatch().Set([]byte("HPF_SIZE"), zero[:])
	db.metadb.CurrBatch().Set([]byte("SEED"), db.seed[:])
	db.metadb.CloseOldBatch()
	return db
}

func NewMoDB(path string) *MoDB {
	metadb, err := indextree.NewRocksDB("rocksdb", path)
	if err != nil {
		panic(err)
	}
	// 8MB Read Buffer, 2GB file block
	hpfile, err := datatree.NewHPFile(8*1024*1024, 2048*1024*1024, path+"/data")
	if err != nil {
		panic(err)
	}
	db := &MoDB{
		path:    path,
		metadb:  metadb,
		hpfile:  &hpfile,
		blkBuf:  make([]byte, 0, 1024),
		idxBuf:  make([]byte, 0, 1024),
		indexer: indexer.New(),
	}
	// for a half-committed block, hpfile may have some garbage after the position
	// marked by HPF_SIZE
	bz := db.metadb.Get([]byte("HPF_SIZE"))
	size := binary.LittleEndian.Uint64(bz)
	err = db.hpfile.Truncate(int64(size))
	if err != nil {
		panic(err)
	}

	// reload the persistent data from metadb into in-memory indexer
	db.reloadToIndexer()

	// hash seed is also saved in metadb. It cannot be changed in MoDB's lifetime
	copy(db.seed[:], db.metadb.Get([]byte("SEED")))

	// If "NEW" key is not deleted, a pending block has not been indexed, so we
	// index it.
	blkBz := db.metadb.Get([]byte("NEW"))
	if blkBz == nil {
		return db
	}
	blk := &types.Block{}
	_, err = blk.UnmarshalMsg(blkBz)
	if err != nil {
		panic(err)
	}
	db.wg.Add(1)
	go db.postAddBlock(blk, -1) //pruneTillHeight==-1 means no prune
	db.wg.Wait() // wait for goroutine to finish
	return db
}

func (db *MoDB) Close() {
	db.wg.Wait() // wait for previous postAddBlock goroutine to finish
	db.hpfile.Close()
	db.metadb.Close()
	db.indexer.Close()
}

// Add a new block for indexing, and prune the index information for blocks before pruneTillHeight
func (db *MoDB) AddBlock(blk *types.Block, pruneTillHeight int64) {
	db.wg.Wait() // wait for previous postAddBlock goroutine to finish
	if(blk == nil) {
		return
	}

	// firstly serialize and write the block into metadb under the key "NEW".
	// if the indexing process is aborted due to crash or something, we
	// can resume the block from metadb
	var err error
	db.blkBuf, err = blk.MarshalMsg(db.blkBuf[:0])
	if err != nil {
		panic(err)
	}
	db.metadb.SetSync([]byte("NEW"), db.blkBuf)

	// start the postAddBlock goroutine which should finish before the next indexing job
	db.wg.Add(1)
	go db.postAddBlock(blk, pruneTillHeight)
	// when this function returns, we are sure that metadb has saved 'blk'
}

// append data at the end of hpfile, padding to 32 bytes
func (db *MoDB) appendToFile(data []byte) int64 {
	var zeros [32]byte
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(len(data)))
	pad := Padding32(4 + len(data))
	off, err := db.hpfile.Append([][]byte{buf[:], data, zeros[:pad]})
	if err != nil {
		panic(err)
	}
	return off/32
}

// post-processing after AddBlock
func (db *MoDB) postAddBlock(blk *types.Block, pruneTillHeight int64) {
	blkIdx := &types.BlockIndex{
		Height:       uint32(blk.Height),
		TxHash48List: make([]uint64, len(blk.TxList)),
		TxPosList:    make([]int64, len(blk.TxList)),
	}
	db.fillLogIndex(blk, blkIdx)
	// Get a write lock before we start updating
	db.mtx.Lock()
	defer func() {
		db.mtx.Unlock()
		db.wg.Done()
	}()

	offset40 := db.appendToFile(blk.BlockInfo)
	blkIdx.BeginOffset = offset40
	blkIdx.BlockHash48 = Sum48(db.seed, blk.BlockHash[:])
	db.indexer.AddBlock(blkIdx.Height, blkIdx.BlockHash48, offset40)

	for i, tx := range blk.TxList {
		offset40 = db.appendToFile(tx.Content)
		blkIdx.TxPosList[i] = offset40
		blkIdx.TxHash48List[i] = Sum48(db.seed, tx.HashId[:])
		id56 := GetId56(blkIdx.Height, i)
		db.indexer.AddTx(id56, blkIdx.TxHash48List[i], offset40)
	}
	for i, addrHash48 := range blkIdx.AddrHashes {
		db.indexer.AddAddr2Log(addrHash48, blkIdx.Height, blkIdx.AddrPosLists[i])
	}
	for i, topicHash48 := range blkIdx.TopicHashes {
		db.indexer.AddTopic2Log(topicHash48, blkIdx.Height, blkIdx.TopicPosLists[i])
	}

	db.metadb.OpenNewBatch()
	// save the index information to metadb, such that we can later recover and prune in-memory index
	var err error
	db.idxBuf, err = blkIdx.MarshalMsg(db.idxBuf[:0])
	if err != nil {
		panic(err)
	}
	buf := []byte("B1234")
	binary.LittleEndian.PutUint32(buf[1:], blkIdx.Height)
	db.metadb.CurrBatch().Set(buf, db.idxBuf)
	// write the size of hpfile to metadb
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], uint64(db.hpfile.Size()))
	db.metadb.CurrBatch().Set([]byte("HPF_SIZE"), b8[:])
	// with blkIdx and hpfile updated, we finish processing the pending block.
	db.metadb.CurrBatch().Delete([]byte("NEW"))
	db.metadb.CloseOldBatch()
	db.hpfile.Flush()
	if db.hpfile.Size() > 192 {
		err = db.hpfile.ReadAt(b8[:], 192, false)
		if err != nil {
			panic(err)
		}
	}
	db.pruneTillBlock(pruneTillHeight)
}

// prune in-memory index and hpfile till the block at 'pruneTillHeight' (not included)
func (db *MoDB) pruneTillBlock(pruneTillHeight int64) {
	if pruneTillHeight < 0 {
		return
	}
	// get an iterator in the range [0, pruneTillHeight)
	start := []byte("B1234")
	binary.LittleEndian.PutUint32(start[1:], 0)
	end := []byte("B1234")
	binary.LittleEndian.PutUint32(end[1:], uint32(pruneTillHeight))
	iter := db.metadb.Iterator(start, end)
	defer iter.Close()
	keys := make([][]byte, 0, 100)
	for iter.Valid() {
		keys = append(keys, iter.Key())
		// get the recorded index information for a block
		bi := &types.BlockIndex{}
		_, err := bi.UnmarshalMsg(iter.Value())
		if err != nil {
			panic(err)
		}
		// now prune in-memory index and hpfile
		db.pruneBlock(bi)
		iter.Next()
	}
	// remove the recorded index information from metadb
	db.metadb.OpenNewBatch()
	for _, key := range keys {
		db.metadb.CurrBatch().Delete(key)
	}
	db.metadb.CloseOldBatch()
}

func (db *MoDB) pruneBlock(bi *types.BlockIndex) {
	// Prune the head part of hpfile
	err := db.hpfile.PruneHead(bi.BeginOffset)
	if err != nil {
		panic(err)
	}
	// Erase the information recorded in 'bi'
	db.indexer.EraseBlock(bi.Height, bi.BlockHash48)
	for i, hash48 := range bi.TxHash48List {
		id56 := GetId56(bi.Height, i)
		db.indexer.EraseTx(id56, hash48, bi.TxPosList[i])
	}
	for _, hash48 := range bi.AddrHashes {
		db.indexer.EraseAddr2Log(hash48, bi.Height)
	}
	for _, hash48 := range bi.TopicHashes {
		db.indexer.EraseTopic2Log(hash48, bi.Height)
	}
}

// fill blkIdx.Topic* and blkIdx.Addr* according to 'blk'
func (db *MoDB) fillLogIndex(blk *types.Block, blkIdx *types.BlockIndex) {
	addrIndex := make(map[uint64][]uint32)
	topicIndex := make(map[uint64][]uint32)
	for i, tx := range blk.TxList {
		for _, log := range tx.LogList {
			for _, topic := range log.Topics {
				topicHash48 := Sum48(db.seed, topic[:])
				AppendAtKey(topicIndex, topicHash48, uint32(i))
			}
			addrHash48 := Sum48(db.seed, log.Address[:])
			AppendAtKey(addrIndex, addrHash48, uint32(i))
		}
	}
	// the map 'addrIndex' is recorded into two slices
	blkIdx.AddrHashes = make([]uint64, 0, len(addrIndex))
	blkIdx.AddrPosLists = make([][]uint32, 0, len(addrIndex))
	for addr, posList := range addrIndex {
		blkIdx.AddrHashes = append(blkIdx.AddrHashes, addr)
		blkIdx.AddrPosLists = append(blkIdx.AddrPosLists, posList)
	}
	// the map 'topicIndex' is recorded into two slices
	blkIdx.TopicHashes = make([]uint64, 0, len(topicIndex))
	blkIdx.TopicPosLists = make([][]uint32, 0, len(topicIndex))
	for topic, posList := range topicIndex {
		blkIdx.TopicHashes = append(blkIdx.TopicHashes, topic)
		blkIdx.TopicPosLists = append(blkIdx.TopicPosLists, posList)
	}
	return
}

// reload index information from metadb into in-memory indexer
func (db *MoDB) reloadToIndexer() {
	// Get an iterator over all recorded blocks' indexes
	start := []byte{byte('B'), 0, 0, 0, 0}
	end := []byte{byte('B'), 255, 255, 255, 255}
	iter := db.metadb.Iterator(start, end)
	defer iter.Close()
	for iter.Valid() {
		bi := &types.BlockIndex{}
		_, err := bi.UnmarshalMsg(iter.Value())
		if err != nil {
			panic(err)
		}
		db.reloadBlockToIndexer(bi)
		iter.Next()
	}
}

// reload one block's index information into in-memory indexer
func (db *MoDB) reloadBlockToIndexer(blkIdx *types.BlockIndex) {
	db.indexer.AddBlock(blkIdx.Height, blkIdx.BlockHash48, blkIdx.BeginOffset)
	for i, txHash48 := range blkIdx.TxHash48List {
		id56 := GetId56(blkIdx.Height, i)
		db.indexer.AddTx(id56, txHash48, blkIdx.TxPosList[i])
	}
	for i, addrHash48 := range blkIdx.AddrHashes {
		db.indexer.AddAddr2Log(addrHash48, blkIdx.Height, blkIdx.AddrPosLists[i])
	}
	for i, topicHash48 := range blkIdx.TopicHashes {
		db.indexer.AddTopic2Log(topicHash48, blkIdx.Height, blkIdx.TopicPosLists[i])
	}
}

// read at offset40*32 to fetch data out
func (db *MoDB) readInFile(offset40 int64) []byte {
	// read the length out
	var buf [4]byte
	offset := GetRealOffset(offset40*32, db.hpfile.Size())
	err := db.hpfile.ReadAt(buf[:], offset, false)
	if err != nil {
		panic(err)
	}
	size := binary.LittleEndian.Uint32(buf[:])
	// read the payload out
	bz := make([]byte, int(size)+4)
	err = db.hpfile.ReadAt(bz, offset, false)
	if err != nil {
		panic(err)
	}
	return bz[4:]
}

// given a block's height, return serialized information.
func (db *MoDB) GetBlockByHeight(height int64) []byte {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	offset40 := db.indexer.GetOffsetByBlockHeight(uint32(height))
	if offset40 < 0 {
		return nil
	}
	return db.readInFile(offset40)
}

// given a transaction's height+index, return serialized information.
func (db *MoDB) GetTxByHeightAndIndex(height int64, index int) []byte {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	id56 := GetId56(uint32(height), index)
	offset40 := db.indexer.GetOffsetByTxID(id56)
	if offset40 < 0 {
		return nil
	}
	return db.readInFile(offset40)
}

// given a block's hash, feed possibly-correct serialized information to collectResult; if
// collectResult confirms the information is correct by returning true, this function stops loop.
func (db *MoDB) GetBlockByHash(hash [32]byte, collectResult func([]byte) bool) {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	hash48 := Sum48(db.seed, hash[:])
	for _, offset40 := range db.indexer.GetOffsetsByBlockHash(hash48) {
		bz := db.readInFile(offset40)
		if collectResult(bz) {
			return
		}
	}
}

// given a block's hash, feed possibly-correct serialized information to collectResult; if
// collectResult confirms the information is correct by returning true, this function stops loop.
func (db *MoDB) GetTxByHash(hash [32]byte, collectResult func([]byte) bool) {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	hash48 := Sum48(db.seed, hash[:])
	for _, offset40 := range db.indexer.GetOffsetsByTxHash(hash48) {
		bz := db.readInFile(offset40)
		if collectResult(bz) {
			return
		}
	}
}

// given 0~1 addr and 0~4 topics, feed the possibly-matching transactions to 'fn'; the return value of 'fn' indicates
// whether it wants more data.
func (db *MoDB) QueryLogs(addr *[20]byte, topics [][32]byte, startHeight, endHeight uint32, fn func([]byte) bool) {
	db.mtx.RLock()
	defer db.mtx.RUnlock()
	addrHash48 := uint64(1) << 63 // an invalid value
	if addr != nil {
		addrHash48 = Sum48(db.seed, (*addr)[:])
	}
	topicHash48List := make([]uint64, len(topics))
	for i, hash := range topics {
		topicHash48List[i] = Sum48(db.seed, hash[:])
	}
	offList := db.indexer.QueryTxOffsets(addrHash48, topicHash48List, startHeight, endHeight)
	for _, offset40 := range offList {
		bz := db.readInFile(offset40)
		if needMore := fn(bz); !needMore {
			break
		}
	}
}

// ===================================

// returns the short hash of the key
func Sum48(seed [8]byte, key []byte) uint64 {
	digest := xxhash.New()
	digest.Write(seed[:])
	digest.Write(key)
	return (digest.Sum64() << 16) >> 16
}

// append value at a slice at 'key'. If the slice does not exist, create it.
func AppendAtKey(m map[uint64][]uint32, key uint64, value uint32) {
	_, ok := m[key]
	if !ok {
		m[key] = make([]uint32, 0, 10)
	}
	m[key] = append(m[key], value)
}

// make sure (length+n)%32 == 0
func Padding32(length int) (n int) {
	mod := length % 32
	if mod != 0 {
		n = 32 - mod
	}
	return
}

// offset40 can represent 32TB range, but a hpfile's virual size can be larger than it.
// calculate a real offset from offset40 which pointing to a valid position in hpfile.
func GetRealOffset(offset, size int64) int64 {
	unit := int64(32) << 40 // 32 tera bytes
	n := size / unit
	if size % unit == 0 {
		n--
	}
	offset += n * unit
	if offset > size {
		offset -= unit
	}
	return offset
}

func GetId56(height uint32, i int) uint64 {
	return (uint64(height) << 24) | uint64(i)
}
