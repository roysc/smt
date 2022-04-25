package smt

import (
	"bytes"
	"hash"
)

var (
	_ treeNode = (*innerNode)(nil)
	_ treeNode = (*leafNode)(nil)
)

type treeNode interface {
	Persisted() bool
	CachedDigest() []byte
}

type innerNode struct {
	leftChild, rightChild treeNode
	persisted             bool
	// Cached hash digest
	digest []byte
}

type leafNode struct {
	path      []byte
	valueHash []byte
	persisted bool
	// Cached hash digest
	digest []byte
}

// represents uncached persisted node
type lazyNode struct {
	digest []byte
}

type SMT struct {
	BaseSMT
	nodes     MapStore
	savedRoot []byte
	// Current state of tree
	tree treeNode
	// Lists of per-operation orphan sets
	orphans []orphanNodes
}

// Hashes of persisted nodes deleted from tree
type orphanNodes = [][]byte

func NewSMT(nodes MapStore, hasher hash.Hash, options ...Option) *SMT {
	smt := SMT{
		BaseSMT: newBaseSMT(hasher),
		nodes:   nodes,
	}
	for _, option := range options {
		option(&smt)
	}
	return &smt
}

func ImportSMT(nodes MapStore, hasher hash.Hash, root []byte, options ...Option) *SMT {
	smt := NewSMT(nodes, hasher, options...)
	smt.tree = &lazyNode{root}
	smt.savedRoot = root
	return smt
}

func (smt *SMT) Get(key []byte) ([]byte, error) {
	path := smt.ph.Path(key)
	var leaf *leafNode
	var err error
	for node, depth := &smt.tree, 0; ; depth++ {
		*node, err = smt.resolveLazy(*node)
		if err != nil {
			return nil, err
		}
		if *node == nil {
			break
		}
		if n, ok := (*node).(*leafNode); ok {
			if bytes.Equal(path, n.path) {
				leaf = n
			}
			break
		}
		inner := (*node).(*innerNode)
		if getBitAtFromMSB(path, depth) == left {
			node = &inner.leftChild
		} else {
			node = &inner.rightChild
		}
	}
	if leaf == nil {
		return defaultValue, nil
	}
	return leaf.valueHash, nil
}

func (smt *SMT) Update(key []byte, value []byte) error {
	path := smt.ph.Path(key)
	valueHash := smt.digestValue(value)
	var orphans orphanNodes
	tree, err := smt.update(smt.tree, 0, path, valueHash, &orphans)
	if err != nil {
		return err
	}
	smt.tree = tree
	smt.orphans = append(smt.orphans, orphans)
	return nil
}

func (smt *SMT) update(
	node treeNode, depth int, path, value []byte, orphans *orphanNodes,
) (treeNode, error) {
	node, err := smt.resolveLazy(node)
	if err != nil {
		return node, err
	}

	newLeaf := &leafNode{path: path, valueHash: value}
	// Empty subtree is always replaced by a single leaf
	if node == nil {
		return newLeaf, nil
	}
	if leaf, ok := node.(*leafNode); ok {
		// TODO (optimization) - can just count [depth:]
		prefixlen := countCommonPrefix(path, leaf.path)
		if prefixlen == smt.depth() { // replace leaf if paths are equal
			smt.addOrphan(orphans, node)
			return newLeaf, nil
		}
		// We must create a "list" of single-branch inner nodes
		var listRoot treeNode
		prev := &listRoot
		for d := depth; d < prefixlen; d++ {
			inner := &innerNode{}
			*prev = inner
			if getBitAtFromMSB(path, d) == left {
				prev = &inner.leftChild
			} else {
				prev = &inner.rightChild
			}
		}
		if getBitAtFromMSB(path, prefixlen) == left {
			*prev = &innerNode{leftChild: newLeaf, rightChild: leaf}
		} else {
			*prev = &innerNode{leftChild: leaf, rightChild: newLeaf}
		}
		return listRoot, nil
	}

	smt.addOrphan(orphans, node)
	var child *treeNode
	inner := node.(*innerNode)
	if getBitAtFromMSB(path, depth) == left {
		child = &inner.leftChild
	} else {
		child = &inner.rightChild
	}
	*child, err = smt.update(*child, depth+1, path, value, orphans)
	if err != nil {
		return node, err
	}
	inner.setDirty()
	return inner, nil
}

func (smt *SMT) Delete(key []byte) error {
	path := smt.ph.Path(key)
	var orphans orphanNodes
	tree, err := smt.delete(smt.tree, 0, path, &orphans)
	if err != nil {
		return err
	}
	smt.tree = tree
	smt.orphans = append(smt.orphans, orphans)
	return nil
}

func (smt *SMT) delete(node treeNode, depth int, path []byte, orphans *orphanNodes,
) (treeNode, error) {
	node, err := smt.resolveLazy(node)
	if err != nil {
		return node, err
	}

	if node == nil {
		return node, ErrKeyNotPresent
	}
	if leaf, ok := node.(*leafNode); ok {
		if !bytes.Equal(path, leaf.path) {
			return node, ErrKeyNotPresent
		}
		smt.addOrphan(orphans, node)
		return nil, nil
	}

	smt.addOrphan(orphans, node)
	var child, sib *treeNode
	inner := node.(*innerNode)
	if getBitAtFromMSB(path, depth) == left {
		child, sib = &inner.leftChild, &inner.rightChild
	} else {
		child, sib = &inner.rightChild, &inner.leftChild
	}
	*child, err = smt.delete(*child, depth+1, path, orphans)
	if err != nil {
		return node, err
	}
	*sib, err = smt.resolveLazy(*sib)
	if err != nil {
		return node, err
	}
	// We can only replace this node with a leaf -
	// Inner nodes exist at a fixed depth, and can't be moved.
	if *child == nil {
		if _, ok := (*sib).(*leafNode); ok {
			return *sib, nil
		}
	}
	if *sib == nil {
		if _, ok := (*child).(*leafNode); ok {
			return *child, nil
		}
	}
	inner.setDirty()
	return inner, nil
}

func (smt *SMT) Prove(key []byte) (proof SparseMerkleProof, err error) {
	path := smt.ph.Path(key)
	var siblings []treeNode
	var sib treeNode

	node := smt.tree
	for depth := 0; depth < smt.depth(); depth++ {
		node, err = smt.resolveLazy(node)
		if err != nil {
			return
		}
		if node == nil {
			break
		}
		if _, ok := node.(*leafNode); ok {
			break
		}
		inner := node.(*innerNode)
		if getBitAtFromMSB(path, depth) == left {
			node, sib = inner.leftChild, inner.rightChild
		} else {
			node, sib = inner.rightChild, inner.leftChild
		}
		siblings = append(siblings, sib)
	}

	// Deal with non-membership proofs. If there is no leaf on this path,
	// we do not need to add anything else to the proof.
	var leafData []byte
	if node != nil {
		leaf := node.(*leafNode)
		if !bytes.Equal(leaf.path, path) {
			// This is a non-membership proof that involves showing a different leaf.
			// Add the leaf data to the proof.
			leafData = encodeLeaf(leaf.path, leaf.valueHash)
		}
	}
	// Hash siblings from bottom up.
	var sideNodes [][]byte
	for i, _ := range siblings {
		var sideNode []byte
		sibling := siblings[len(siblings)-1-i]
		sideNode = smt.hashNode(sibling)
		sideNodes = append(sideNodes, sideNode)
	}

	proof = SparseMerkleProof{
		SideNodes:             sideNodes,
		NonMembershipLeafData: leafData,
	}
	if sib != nil {
		sib, err = smt.resolveLazy(sib)
		if err != nil {
			return
		}
		proof.SiblingData = smt.serialize(sib)
	}
	return
}

func (smt *SMT) recursiveLoad(hash []byte) (treeNode, error) {
	return smt.resolve(hash, smt.recursiveLoad)
}

// resolves a stub into a cached node
func (smt *SMT) resolveLazy(node treeNode) (treeNode, error) {
	stub, ok := node.(*lazyNode)
	if !ok {
		return node, nil
	}
	resolver := func(hash []byte) (treeNode, error) {
		return &lazyNode{hash}, nil
	}
	return smt.resolve(stub.digest, resolver)
}

func (smt *SMT) resolve(hash []byte, resolver func([]byte) (treeNode, error),
) (ret treeNode, err error) {
	if bytes.Equal(smt.th.placeholder(), hash) {
		return
	}
	data, err := smt.nodes.Get(hash)
	if err != nil {
		return
	}
	if isLeaf(data) {
		leaf := leafNode{persisted: true, digest: hash}
		leaf.path, leaf.valueHash = parseLeaf(data, smt.ph)
		return &leaf, nil
	}
	leftHash, rightHash := smt.th.parseNode(data)
	inner := innerNode{persisted: true, digest: hash}
	inner.leftChild, err = resolver(leftHash)
	if err != nil {
		return
	}
	inner.rightChild, err = resolver(rightHash)
	if err != nil {
		return
	}
	return &inner, nil
}

func (smt *SMT) Save() (err error) {
	if err = smt.save(smt.tree, 0); err != nil {
		return
	}
	// All orphans are persisted and have cached digests, so we don't need to check for null
	for _, orphans := range smt.orphans {
		for _, hash := range orphans {
			if err = smt.nodes.Delete(hash); err != nil {
				return
			}
		}
	}
	smt.orphans = nil
	smt.savedRoot = smt.Root()
	return
}

func (smt *SMT) save(node treeNode, depth int) error {
	if node != nil && node.Persisted() {
		return nil
	}
	switch n := node.(type) {
	case *leafNode:
		n.persisted = true
	case *innerNode:
		n.persisted = true
		if err := smt.save(n.leftChild, depth+1); err != nil {
			return err
		}
		if err := smt.save(n.rightChild, depth+1); err != nil {
			return err
		}
	default:
		return nil
	}
	return smt.nodes.Set(smt.hashNode(node), smt.serialize(node))
}

func (smt *SMT) Root() []byte {
	return smt.hashNode(smt.tree)
}

func (node *leafNode) Persisted() bool  { return node.persisted }
func (node *innerNode) Persisted() bool { return node.persisted }
func (node *lazyNode) Persisted() bool  { return true }

func (node *leafNode) CachedDigest() []byte  { return node.digest }
func (node *innerNode) CachedDigest() []byte { return node.digest }
func (node *lazyNode) CachedDigest() []byte  { return node.digest }

func (inner *innerNode) setDirty() {
	inner.persisted = false
	inner.digest = nil
}

func (smt *SMT) serialize(node treeNode) (data []byte) {
	switch n := node.(type) {
	case *lazyNode:
		panic("serialize(lazyNode)")
	case *leafNode:
		return encodeLeaf(n.path, n.valueHash)
	case *innerNode:
		var lh, rh []byte
		lh = smt.hashNode(n.leftChild)
		rh = smt.hashNode(n.rightChild)
		return encodeInner(lh, rh)
	}
	return nil
}

func (smt *SMT) hashNode(node treeNode) []byte {
	if node == nil {
		return smt.th.placeholder()
	}
	var cache *[]byte
	switch n := node.(type) {
	case *lazyNode:
		return n.digest
	case *leafNode:
		cache = &n.digest
	case *innerNode:
		cache = &n.digest
	}
	if *cache == nil {
		*cache = smt.th.digest(smt.serialize(node))
	}
	return *cache
}

func (smt *SMT) addOrphan(orphans *[][]byte, node treeNode) {
	if node.Persisted() {
		*orphans = append(*orphans, node.CachedDigest())
	}
}
