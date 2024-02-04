package iavl

import (
	"bytes"
	"errors"
	"fmt"

	db "github.com/cometbft/cometbft-db"
)

// maxBatchSize is the maximum size of the import batch before flushing it to the database
const maxBatchSize = 10000

// ErrNoImport is returned when calling methods on a closed importer
var ErrNoImport = errors.New("no import in progress")

// Importer imports data into an empty MutableTree. It is created by MutableTree.Import(). Users
// must call Close() when done.
//
// ExportNodes must be imported in the order returned by Exporter, i.e. depth-first post-order (LRN).
//
// Importer is not concurrency-safe, it is the caller's responsibility to ensure the tree is not
// modified while performing an import.
type Importer struct {
	tree      *MutableTree
	version   int64
	batch     db.Batch
	batchSize uint32
	stack     []*Node

	// Optimistic raw key value import
	optimistic bool
}

// newImporter creates a new Importer for an empty MutableTree.
//
// version should correspond to the version that was initially exported. It must be greater than
// or equal to the highest ExportNode version number given.
func newImporter(tree *MutableTree, version int64) (*Importer, error) {
	return newImporterWithOptions(tree, version, false)
}

// newOptimisticImporter creates a new Importer for an empty MutableTree.
//
// expects optimistic raw key values for import
func newOptimisticImporter(tree *MutableTree, version int64) (*Importer, error) {
	return newImporterWithOptions(tree, version, true)
}

func newImporterWithOptions(tree *MutableTree, version int64, optimistic bool) (*Importer, error) {
	if version < 0 {
		return nil, errors.New("imported version cannot be negative")
	}
	if tree.ndb.latestVersion > 0 {
		return nil, fmt.Errorf("found database at version %d, must be 0", tree.ndb.latestVersion)
	}
	if !tree.IsEmpty() {
		return nil, errors.New("tree must be empty")
	}

	return &Importer{
		tree:       tree,
		version:    version,
		batch:      tree.ndb.db.NewBatch(),
		stack:      make([]*Node, 0, 8),
		optimistic: optimistic,
	}, nil
}

// Close frees all resources. It is safe to call multiple times. Uncommitted nodes may already have
// been flushed to the database, but will not be visible.
func (i *Importer) Close() {
	if i.batch != nil {
		i.batch.Close()
	}
	i.batch = nil
	i.tree = nil
}

// sendBatchIfFull can be called during imports after each key add
// automatically batch.Write() when pending writes > maxBatchSize
func (i *Importer) sendBatchIfFull() error {
	if i.batchSize >= maxBatchSize {
		// Wait for previous batch.
		var err error
		if i.inflightCommit != nil {
			err = <-i.inflightCommit
			i.inflightCommit = nil
		}
		if err != nil {
			return err
		}
		result := make(chan error)
		i.inflightCommit = result
		go func(batch db.Batch) {
			defer batch.Close()
			result <- batch.Write()
		}(i.batch)
		i.batch = i.tree.ndb.db.NewBatch()
		i.batchSize = 0
	}

	return nil
}

// OptimisticAdd adds a TRUSTED leveldb key value pair WITHOUT verification
func (i *Importer) OptimisticAdd(exportNode *ExportNode) error {
	if i.tree == nil {
		return ErrNoImport
	}
	if exportNode == nil {
		return errors.New("node cannot be nil")
	}
	if exportNode.Key == nil {
		return errors.New("node.Key cannot be nil")
	}
	if exportNode.Value == nil {
		return errors.New("node.Value cannot be nil")
	}

	if err := i.batch.Set(i.tree.ndb.nodeKey(exportNode.Key), exportNode.Value); err != nil {
		return err
	}
	i.batchSize++

	if err := i.sendBatchIfFull(); err != nil {
		return errors.New("failed sending db write batch")
	}

	return nil
}

// Add adds an ExportNode to the import. ExportNodes must be added in the order returned by
// Exporter, i.e. depth-first post-order (LRN). Nodes are periodically flushed to the database,
// but the imported version is not visible until Commit() is called.
func (i *Importer) Add(exportNode *ExportNode) error {
	// Keep the same Add(node) API but run faster optimistic import when configured
	if i.optimistic {
		return i.OptimisticAdd(exportNode)
	}
	if i.tree == nil {
		return ErrNoImport
	}
	if exportNode == nil {
		return errors.New("node cannot be nil")
	}
	if exportNode.Version > i.version {
		return fmt.Errorf("node version %v can't be greater than import version %v",
			exportNode.Version, i.version)
	}

	node := &Node{
		key:           exportNode.Key,
		value:         exportNode.Value,
		version:       exportNode.Version,
		subtreeHeight: exportNode.Height,
	}

	// We build the tree from the bottom-left up. The stack is used to store unresolved left
	// children while constructing right children. When all children are built, the parent can
	// be constructed and the resolved children can be discarded from the stack. Using a stack
	// ensures that we can handle additional unresolved left children while building a right branch.
	//
	// We don't modify the stack until we've verified the built node, to avoid leaving the
	// importer in an inconsistent state when we return an error.
	stackSize := len(i.stack)
	switch {
	case stackSize >= 2 && i.stack[stackSize-1].subtreeHeight < node.subtreeHeight && i.stack[stackSize-2].subtreeHeight < node.subtreeHeight:
		node.leftNode = i.stack[stackSize-2]
		node.leftHash = node.leftNode.hash
		node.rightNode = i.stack[stackSize-1]
		node.rightHash = node.rightNode.hash
	case stackSize >= 1 && i.stack[stackSize-1].subtreeHeight < node.subtreeHeight:
		node.leftNode = i.stack[stackSize-1]
		node.leftHash = node.leftNode.hash
	}

	if node.subtreeHeight == 0 {
		node.size = 1
	}
	if node.leftNode != nil {
		node.size += node.leftNode.size
	}
	if node.rightNode != nil {
		node.size += node.rightNode.size
	}

	_, err := node._hash()
	if err != nil {
		return err
	}

	err = node.validate()
	if err != nil {
		return err
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	if err = node.writeBytes(buf); err != nil {
		return err
	}

	bytesCopy := make([]byte, buf.Len())
	copy(bytesCopy, buf.Bytes())

	if err = i.batch.Set(i.tree.ndb.nodeKey(node.hash), bytesCopy); err != nil {
		return err
	}

	i.batchSize++
	if i.batchSize >= maxBatchSize {
		err = i.batch.Write()
		if err != nil {
			return err
		}
		i.batch.Close()
		i.batch = i.tree.ndb.db.NewBatch()
		i.batchSize = 0
	}

	// Update the stack now that we know there were no errors
	switch {
	case node.leftHash != nil && node.rightHash != nil:
		i.stack = i.stack[:stackSize-2]
	case node.leftHash != nil || node.rightHash != nil:
		i.stack = i.stack[:stackSize-1]
	}
	// Only hash\height\size of the node will be used after it be pushed into the stack.
	i.stack = append(i.stack, &Node{hash: node.hash, subtreeHeight: node.subtreeHeight, size: node.size})

	return nil
}

// Commit finalizes the import by flushing any outstanding nodes to the database, making the
// version visible, and updating the tree metadata. It can only be called once, and calls Close()
// internally.
func (i *Importer) Commit() error {
	if i.tree == nil {
		return ErrNoImport
	}

	// Optimistic: All keys should be already imported
	if !i.optimistic {
		switch len(i.stack) {
		case 0:
			if err := i.batch.Set(i.tree.ndb.rootKey(i.version), []byte{}); err != nil {
				return err
			}
		case 1:
			if err := i.batch.Set(i.tree.ndb.rootKey(i.version), i.stack[0].hash); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid node structure, found stack size %v when committing",
				len(i.stack))
		}
	}

	err := i.batch.WriteSync()
	if err != nil {
		return err
	}
	i.tree.ndb.resetLatestVersion(i.version)

	_, err = i.tree.LoadVersion(i.version)
	if err != nil {
		return err
	}

	i.Close()
	return nil
}
