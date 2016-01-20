package agro

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/net/context"

	"github.com/coreos/agro/models"
)

type (
	// VolumeID represents a unique identifier for a Volume.
	VolumeID uint64

	// IndexID represents a unique identifier for an Index.
	IndexID uint64

	// INodeID represents a unique identifier for an INode.
	INodeID uint64
)

// Store is the interface that represents methods that should be common across
// all types of storage providers.
type Store interface {
	Kind() string
	Flush() error
	Close() error
}

// INodeRef is a reference to a unique INode in the filesystem.
type INodeRef struct {
	Volume VolumeID
	INode  INodeID
}

func (i INodeRef) String() string {
	return fmt.Sprintf("vol: %d, inode: %d", i.Volume, i.INode)
}

func (i INodeRef) ToProto() *models.INodeRef {
	return &models.INodeRef{
		Volume: uint64(i.Volume),
		INode:  uint64(i.INode),
	}
}

const INodeRefByteSize = 8 * 2

func (i INodeRef) ToBytes() []byte {
	buf := bytes.NewBuffer(make([]byte, 0, INodeRefByteSize))
	binary.Write(buf, binary.LittleEndian, i)
	out := buf.Bytes()
	if len(out) != INodeRefByteSize {
		panic("breaking contract -- must make size appropriate")
	}
	return out
}

// BlockRef is the identifier for a unique block in the filesystem.
type BlockRef struct {
	INodeRef
	Index IndexID
}

const BlockRefByteSize = 8 * 3

func (b BlockRef) ToBytes() []byte {
	buf := bytes.NewBuffer(make([]byte, 0, BlockRefByteSize))
	binary.Write(buf, binary.LittleEndian, b)
	out := buf.Bytes()
	if len(out) != BlockRefByteSize {
		panic("breaking contract -- must make size appropriate")
	}
	return out
}

func BlockRefFromBytes(b []byte) BlockRef {
	buf := bytes.NewBuffer(b)
	out := BlockRef{}
	binary.Read(buf, binary.LittleEndian, &out)
	return out
}

func (b BlockRef) ToProto() *models.BlockRef {
	return &models.BlockRef{
		Volume: uint64(b.Volume),
		INode:  uint64(b.INode),
		Block:  uint64(b.Index),
	}
}

func BlockFromProto(p *models.BlockRef) BlockRef {
	return BlockRef{
		INodeRef: INodeRef{
			Volume: VolumeID(p.Volume),
			INode:  INodeID(p.INode),
		},
		Index: IndexID(p.Block),
	}
}

func INodeFromProto(p *models.INodeRef) INodeRef {
	return INodeRef{
		Volume: VolumeID(p.Volume),
		INode:  INodeID(p.INode),
	}
}

func (b BlockRef) String() string {
	i := b.INodeRef
	return fmt.Sprintf("vol: %d, inode: %d, block: %d", i.Volume, i.INode, b.Index)
}

func (b BlockRef) IsINode(i INodeRef) bool {
	return b.INode == i.INode && b.Volume == i.Volume
}

// BlockStore is the interface representing the standardized methods to
// interact with something storing blocks.
type BlockStore interface {
	Store
	GetBlock(ctx context.Context, b BlockRef) ([]byte, error)
	WriteBlock(ctx context.Context, b BlockRef, data []byte) error
	DeleteBlock(ctx context.Context, b BlockRef) error
	DeleteINodeBlocks(ctx context.Context, b INodeRef) error
	NumBlocks() uint64
	UsedBlocks() uint64
	BlockIterator() BlockIterator
	ReplaceBlockStore(BlockStore) (BlockStore, error)
	// TODO(barakmich) FreeBlocks()
}

type BlockIterator interface {
	Err() error
	Next() bool
	BlockRef() BlockRef
	Close() error
}

type NewBlockStoreFunc func(string, Config, GlobalMetadata) (BlockStore, error)

var blockStores map[string]NewBlockStoreFunc

func RegisterBlockStore(name string, newFunc NewBlockStoreFunc) {
	if blockStores == nil {
		blockStores = make(map[string]NewBlockStoreFunc)
	}

	if _, ok := blockStores[name]; ok {
		panic("agro: attempted to register BlockStore " + name + " twice")
	}

	blockStores[name] = newFunc
}

func CreateBlockStore(kind string, name string, cfg Config, gmd GlobalMetadata) (BlockStore, error) {
	clog.Infof("creating blockstore: %s", kind)
	return blockStores[kind](name, cfg, gmd)
}

// INodeStore is the interface representing the standardized methods to
// interact with something storing INodes.
type INodeStore interface {
	Store
	GetINode(ctx context.Context, i INodeRef) (*models.INode, error)
	WriteINode(ctx context.Context, i INodeRef, inode *models.INode) error
	DeleteINode(ctx context.Context, i INodeRef) error
	INodeIterator() INodeIterator
	ReplaceINodeStore(INodeStore) (INodeStore, error)
}

type INodeIterator interface {
	Err() error
	Next() bool
	INodeRef() INodeRef
	Close() error
}

type NewINodeStoreFunc func(string, Config) (INodeStore, error)

var inodeStores map[string]NewINodeStoreFunc

func RegisterINodeStore(name string, newFunc NewINodeStoreFunc) {
	if inodeStores == nil {
		inodeStores = make(map[string]NewINodeStoreFunc)
	}

	if _, ok := inodeStores[name]; ok {
		panic("agro: attempted to register INodeStore " + name + " twice")
	}

	inodeStores[name] = newFunc
}

func CreateINodeStore(kind string, name string, cfg Config) (INodeStore, error) {
	clog.Infof("creating inode store: %s", kind)
	return inodeStores[kind](name, cfg)
}
