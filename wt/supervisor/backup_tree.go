package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"sort"
)

// readTreeLocked reads an app's index, starting empty when the file is missing or broken.
func (m *backupManager) readTreeLocked(app string) (backupTree, bool, error) {
	var changed bool

	tree := backupTree{
		Nodes: map[string]backupNode{},
	}

	switch data, err := os.ReadFile(m.treePath(app)); {
	case err == nil:
		if err := json.Unmarshal(data, &tree); err != nil {
			tree = backupTree{
				Nodes: map[string]backupNode{},
			}
			changed = true
		}
	case os.IsNotExist(err):
		changed = true
	default:
		return tree, false, err
	}

	if tree.Nodes == nil {
		tree.Nodes = map[string]backupNode{}
		changed = true
	}

	return tree, changed, nil
}

// saveTreeLocked writes the tree atomically while the caller holds the app lock.
func (m *backupManager) saveTreeLocked(app string, tree backupTree) error {
	if tree.Nodes == nil {
		tree.Nodes = map[string]backupNode{}
	}

	if err := os.MkdirAll(m.appDir(app), 0o755); err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}

	data, err := json.Marshal(tree)
	if err != nil {
		return err
	}

	if err := writeFileAtomic(m.treePath(app), append(data, '\n')); err != nil {
		return fmt.Errorf("write backup tree: %w", err)
	}

	return nil
}

// backupOldestLeaf returns the oldest node with no children outside the protected branch.
func backupOldestLeaf(tree backupTree, protected map[string]struct{}) (oldest string) {
	children := map[string]int{}

	for _, node := range tree.Nodes {
		if node.Parent != "" {
			children[node.Parent]++
		}
	}

	for name := range tree.Nodes {
		if _, ok := protected[name]; ok {
			continue
		}

		if children[name] > 0 {
			continue
		}

		if oldest == "" || name < oldest {
			oldest = name
		}
	}

	return
}

// backupOldestRoot returns the lexicographically oldest root, which is also the oldest snapshot.
func backupOldestRoot(tree backupTree) (root string) {
	for name, node := range tree.Nodes {
		if node.Parent != "" {
			continue
		}

		if root == "" || name < root {
			root = name
		}
	}

	return
}

// adoptSingleRoot makes the oldest root the only one, so the graph starts in one place.
func adoptSingleRoot(tree *backupTree) (changed bool) {
	root := backupOldestRoot(*tree)

	for name, node := range tree.Nodes {
		if node.Parent == "" && name != root {
			node.Parent = root
			tree.Nodes[name] = node
			changed = true
		}
	}

	return
}

// backupPathChild returns the child of root that lies on the protected branch.
func backupPathChild(tree backupTree, root string, protected map[string]struct{}) string {
	for name, node := range tree.Nodes {
		if node.Parent != root {
			continue
		}

		if _, ok := protected[name]; ok {
			return name
		}
	}

	return ""
}

// backupPathNames is the current node and its ancestors, which pruning must not remove while another choice exists.
func backupPathNames(tree backupTree) map[string]struct{} {
	protected := map[string]struct{}{}

	for id := tree.Current; id != ""; {
		if _, seen := protected[id]; seen {
			break
		}

		protected[id] = struct{}{}
		node, ok := tree.Nodes[id]

		if !ok {
			break
		}

		id = node.Parent
	}

	return protected
}

// pruneLocked removes the oldest snapshots beyond keep, keeping the branch the live database follows for as long as possible. Abandoned leaves go first; only when the current branch alone is too long is its oldest node dropped and its child promoted to root.
func (m *backupManager) pruneLocked(app string, keep int) error {
	state, err := m.stateLocked(app)
	if err != nil {
		return err
	}

	tree := state.Tree
	if len(tree.Nodes) <= keep {
		return nil
	}

	var changed bool

	for len(tree.Nodes) > keep {
		protected := backupPathNames(tree)

		if leaf := backupOldestLeaf(tree, protected); leaf != "" {
			if err := m.removeSnapshot(app, leaf); err != nil {
				return err
			}

			delete(tree.Nodes, leaf)

			changed = true

			continue
		}

		root := backupOldestRoot(tree)
		if root == "" || root == tree.Current {
			break
		}

		child := backupPathChild(tree, root, protected)

		if err := m.removeSnapshot(app, root); err != nil {
			return err
		}

		delete(tree.Nodes, root)

		if child != "" {
			node := tree.Nodes[child]
			node.Parent = ""
			tree.Nodes[child] = node
		}

		changed = true
	}

	if changed {
		if err := m.saveTreeLocked(app, tree); err != nil {
			return err
		}
	}

	return m.gcBlobs(app)
}

// backupTreeAcyclic reports whether every parent chain ends at a root instead of looping.
func backupTreeAcyclic(tree backupTree) bool {
	for name := range tree.Nodes {
		seen := map[string]struct{}{}

		for id := name; id != ""; {
			if _, ok := seen[id]; ok {
				return false
			}

			seen[id] = struct{}{}
			node, ok := tree.Nodes[id]

			if !ok {
				break
			}

			id = node.Parent
		}
	}

	return true
}

// chainMissingNodes links snapshots the index has not recorded to the current node, so a file written just before a crash still has a place in the tree.
func chainMissingNodes(tree *backupTree, chronological []backupInfo) (changed bool) {
	parent := tree.Current

	if len(tree.Nodes) == 0 {
		parent = ""
	} else if _, ok := tree.Nodes[parent]; !ok {
		parent = ""
	}

	for _, file := range chronological {
		if _, ok := tree.Nodes[file.Name]; ok {
			continue
		}

		tree.Nodes[file.Name] = backupNode{
			Parent: parent,
			Kind: backupKindLegacy,
		}
		parent = file.Name
		changed = true
	}

	return
}

// flattenTree rebuilds a broken index in creation order, keeping each node's kind.
func flattenTree(tree *backupTree, chronological []backupInfo) {
	var parent string

	for _, file := range chronological {
		tree.Nodes[file.Name] = backupNode{
			Parent: parent,
			Kind: tree.Nodes[file.Name].Kind,
		}
		parent = file.Name
	}
}

// repairParentLinks detaches nodes whose parent was pruned or never existed.
func repairParentLinks(tree *backupTree) (changed bool) {
	for name, node := range tree.Nodes {
		if node.Parent == "" {
			continue
		}

		if _, ok := tree.Nodes[node.Parent]; !ok {
			node.Parent = ""
			tree.Nodes[name] = node
			changed = true
		}
	}

	return
}

// syncCurrentNode moves the current marker to the newest snapshot when it no longer names one.
func syncCurrentNode(tree *backupTree, chronological []backupInfo) bool {
	if _, ok := tree.Nodes[tree.Current]; ok {
		return false
	}

	tree.Current = ""

	if len(chronological) > 0 {
		tree.Current = chronological[len(chronological)-1].Name
	}

	return true
}

// reconcileTree makes the index match the files on disk and reports whether it changed.
func reconcileTree(tree *backupTree, files []backupInfo) (changed bool) {
	existing := make(map[string]struct{}, len(files))

	for _, file := range files {
		existing[file.Name] = struct{}{}
	}

	for name := range tree.Nodes {
		if _, ok := existing[name]; !ok {
			delete(tree.Nodes, name)

			changed = true
		}
	}

	chronological := append([]backupInfo(nil), files...)

	sort.Slice(chronological, func(i, j int) bool {
		return chronological[i].CreatedAt.Before(chronological[j].CreatedAt)
	})

	if chainMissingNodes(tree, chronological) {
		changed = true
	}

	if repairParentLinks(tree) {
		changed = true
	}

	if !backupTreeAcyclic(*tree) {
		flattenTree(tree, chronological)

		changed = true
	}

	if adoptSingleRoot(tree) {
		changed = true
	}

	if syncCurrentNode(tree, chronological) {
		changed = true
	}

	return
}

// loadTreeLocked loads snapshot files and their tree index together, rebuilding the index when it is missing, stale or broken.
func (m *backupManager) loadTreeLocked(app string, files []backupInfo) (backupTree, bool, error) {
	tree, changed, err := m.readTreeLocked(app)
	if err != nil {
		return tree, false, err
	}

	return tree, reconcileTree(&tree, files) || changed, nil
}
