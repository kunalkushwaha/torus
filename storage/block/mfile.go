package block

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"runtime"
	"sync"

	"golang.org/x/net/context"

	"github.com/coreos/agro"
	"github.com/coreos/agro/storage"
	"github.com/coreos/pkg/capnslog"
	"github.com/hashicorp/go-immutable-radix"
)

var _ agro.BlockStore = &mfileBlock{}

func init() {
	agro.RegisterBlockStore("mfile", newMFileBlockStore)
}

type mfileBlock struct {
	mut       sync.RWMutex
	data      *storage.MFile
	blockMap  *storage.MFile
	blockTrie *iradix.Tree
	closed    bool
	lastFree  int
	size      uint64
	dfilename string
	mfilename string
	// NB: Still room for improvement. Free lists, smart allocation, etc.
}

func loadTrie(m *storage.MFile) (*iradix.Tree, uint64, error) {
	t := iradix.New()
	tx := t.Txn()
	clog.Infof("loading trie...")
	size := uint64(0)
	var membefore uint64
	if clog.LevelAt(capnslog.DEBUG) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		membefore = mem.Alloc
	}

	blank := make([]byte, agro.BlockRefByteSize)
	for i := uint64(0); i < m.NumBlocks(); i++ {
		b := m.GetBlock(i)
		if bytes.Equal(blank, b) {
			continue
		}
		tx.Insert(b, int(i))
		size++
	}
	if clog.LevelAt(capnslog.DEBUG) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		clog.Debugf("trie memory usage: %dK", ((mem.Alloc - membefore) / 1024))
	}
	clog.Infof("done loading trie")
	return tx.Commit(), size, nil
}

func newMFileBlockStore(name string, cfg agro.Config, meta agro.GlobalMetadata) (agro.BlockStore, error) {
	nBlocks := cfg.StorageSize / meta.BlockSize
	dpath := filepath.Join(cfg.DataDir, "block", fmt.Sprintf("data-%s.blk", name))
	mpath := filepath.Join(cfg.DataDir, "block", fmt.Sprintf("map-%s.blk", name))
	d, err := storage.CreateOrOpenMFile(dpath, cfg.StorageSize, meta.BlockSize)
	if err != nil {
		return nil, err
	}
	m, err := storage.CreateOrOpenMFile(mpath, nBlocks*agro.BlockRefByteSize, agro.BlockRefByteSize)
	if err != nil {
		return nil, err
	}
	trie, size, err := loadTrie(m)
	if err != nil {
		return nil, err
	}
	if m.NumBlocks() != d.NumBlocks() {
		panic("non-equal number of blocks between data and metadata")
	}
	return &mfileBlock{
		data:      d,
		blockMap:  m,
		blockTrie: trie,
		size:      size,
		dfilename: dpath,
		mfilename: mpath,
	}, nil
}

func (m *mfileBlock) Kind() string { return "mfile" }
func (m *mfileBlock) NumBlocks() uint64 {
	return m.data.NumBlocks()
}

func (m *mfileBlock) UsedBlocks() uint64 {
	return m.size
}

func (m *mfileBlock) Flush() error {
	err := m.data.Flush()

	if err != nil {
		return err
	}
	err = m.blockMap.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (m *mfileBlock) Close() error {
	m.mut.Lock()
	defer m.mut.Unlock()
	return m.close()
}

func (m *mfileBlock) close() error {
	m.Flush()
	err := m.data.Close()
	if err != nil {
		return err
	}
	err = m.blockMap.Close()
	if err != nil {
		return err
	}
	m.closed = true
	return nil
}

func (m *mfileBlock) findIndex(s agro.BlockRef) int {
	id := s.ToBytes()
	clog.Tracef("finding blockid %s, bytes %v", s, id)
	if v, ok := m.blockTrie.Get(id); ok {
		return v.(int)
	}
	return -1
}

func (m *mfileBlock) findEmpty() int {
	emptyBlock := make([]byte, agro.BlockRefByteSize)
	for i := uint64(0); i < m.NumBlocks(); i++ {
		b := m.blockMap.GetBlock((i + uint64(m.lastFree) + 1) % m.NumBlocks())
		if bytes.Equal(b, emptyBlock) {
			m.lastFree = int((i + uint64(m.lastFree) + 1) % m.NumBlocks())
			return m.lastFree
		}
	}
	return -1
}

func (m *mfileBlock) GetBlock(_ context.Context, s agro.BlockRef) ([]byte, error) {
	m.mut.RLock()
	defer m.mut.RUnlock()
	if m.closed {
		return nil, agro.ErrClosed
	}
	index := m.findIndex(s)
	if index == -1 {
		return nil, agro.ErrBlockNotExist
	}
	clog.Tracef("mfile: getting block at index %d", index)
	return m.data.GetBlock(uint64(index)), nil
}

func (m *mfileBlock) WriteBlock(_ context.Context, s agro.BlockRef, data []byte) error {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.closed {
		return agro.ErrClosed
	}
	index := m.findEmpty()
	if index == -1 {
		clog.Error("mfile: out of space")
		return agro.ErrOutOfSpace
	}
	clog.Tracef("mfile: writing block at index %d", index)
	err := m.data.WriteBlock(uint64(index), data)
	if err != nil {
		return err
	}
	err = m.blockMap.WriteBlock(uint64(index), s.ToBytes())
	if err != nil {
		return err
	}

	tx := m.blockTrie.Txn()
	_, exists := tx.Insert(s.ToBytes(), index)
	if exists {
		return errors.New("mfile: block already existed?")
	}
	m.size++
	m.blockTrie = tx.Commit()
	return nil
}

func (m *mfileBlock) DeleteBlock(_ context.Context, s agro.BlockRef) error {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.closed {
		return agro.ErrClosed
	}
	index := m.findIndex(s)
	if index == -1 {
		return agro.ErrBlockNotExist
	}
	err := m.blockMap.WriteBlock(uint64(index), make([]byte, agro.BlockRefByteSize))
	if err != nil {
		return err
	}
	tx := m.blockTrie.Txn()
	_, exists := tx.Delete(s.ToBytes())
	if !exists {
		return errors.New("mfile: deleting non-existent thing?")
	}
	m.size--
	m.blockTrie = tx.Commit()
	return nil
}

func (m *mfileBlock) DeleteINodeBlocks(_ context.Context, s agro.INodeRef) error {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.closed {
		return agro.ErrClosed
	}
	tx := m.blockTrie.Txn()
	it := tx.Root().Iterator()
	it.SeekPrefix(s.ToBytes())
	var keyList [][]byte
	var deleteList []int
	for {
		key, value, ok := it.Next()
		if !ok {
			break
		}
		deleteList = append(deleteList, value.(int))
		keyList = append(keyList, key)
	}
	for _, v := range deleteList {
		err := m.blockMap.WriteBlock(uint64(v), make([]byte, agro.BlockRefByteSize))
		if err != nil {
			return err
		}
	}
	for _, k := range keyList {
		tx.Delete(k)
	}
	m.size = m.size - uint64(len(deleteList))
	m.blockTrie = tx.Commit()
	return nil
}

func (m *mfileBlock) ReplaceBlockStore(bs agro.BlockStore) (agro.BlockStore, error) {
	newM, ok := bs.(*mfileBlock)
	if !ok {
		return nil, errors.New("not replacing an mfileBlockStore")
	}
	m.mut.Lock()
	defer m.mut.Unlock()
	newM.mut.Lock()
	defer newM.mut.Unlock()
	err := os.Remove(m.dfilename)
	if err != nil {
		return nil, err
	}
	err = os.Remove(m.mfilename)
	if err != nil {
		return nil, err
	}
	err = os.Rename(newM.mfilename, m.mfilename)
	if err != nil {
		return nil, err
	}
	err = os.Rename(newM.dfilename, m.dfilename)
	if err != nil {
		return nil, err
	}
	out := &mfileBlock{
		data:      newM.data,
		blockMap:  newM.blockMap,
		blockTrie: newM.blockTrie,
		lastFree:  newM.lastFree,
		size:      newM.size,
		dfilename: m.dfilename,
		mfilename: m.mfilename,
	}
	newM.data = nil
	newM.blockMap = nil
	err = m.close()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (m *mfileBlock) BlockIterator() agro.BlockIterator {
	m.mut.Lock()
	defer m.mut.Unlock()
	return &mfileIterator{
		tx: m.blockTrie.Txn(),
	}
}

type mfileIterator struct {
	tx     *iradix.Txn
	err    error
	it     *iradix.Iterator
	result []byte
}

func (i *mfileIterator) Err() error { return i.err }

func (i *mfileIterator) Next() bool {
	if i.err != nil || i.tx == nil {
		return false
	}
	if i.it == nil {
		i.it = i.tx.Root().Iterator()
	}
	var ok bool
	i.result, _, ok = i.it.Next()
	return ok
}

func (i *mfileIterator) BlockRef() agro.BlockRef {
	return agro.BlockRefFromBytes(i.result)
}

func (i *mfileIterator) Close() error {
	i.tx = nil
	return nil
}
