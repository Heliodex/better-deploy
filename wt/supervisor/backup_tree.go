package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

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

func backupOldestRoot(tree backupTree) string {
	root := ""
	for name, node := range tree.Nodes {
		if node.Parent != "" {
			continue
		}
		if root == "" || name < root {
			root = name
		}
	}
	return root
}

// readTreeLocked reads an app's index, starting empty when the file is missing or broken.
func (m *backupManager) readTreeLocked(app string) (backupTree, bool, error) {
	tree := backupTree{Nodes: map[string]backupNode{}}
	changed := false

	data, err := os.ReadFile(m.treePath(app))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &tree); err != nil {
			tree = backupTree{Nodes: map[string]backupNode{}}
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

// chainMissingNodes links snapshots the index has not recorded to the current node, so a file written just before a crash still has a place in the tree.
func chainMissingNodes(tree *backupTree, chronological []backupInfo) bool {
	changed := false
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
		tree.Nodes[file.Name] = backupNode{Parent: parent, Kind: backupKindLegacy}
		parent = file.Name
		changed = true
	}
	return changed
}

// repairParentLinks detaches nodes whose parent was pruned or never existed.
func repairParentLinks(tree *backupTree) bool {
	changed := false
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
	return changed
}

// flattenTree rebuilds a broken index in creation order, keeping each node's kind.
func flattenTree(tree *backupTree, chronological []backupInfo) {
	parent := ""
	for _, file := range chronological {
		tree.Nodes[file.Name] = backupNode{Parent: parent, Kind: tree.Nodes[file.Name].Kind}
		parent = file.Name
	}
}

// adoptSingleRoot makes the oldest root the only one, so the graph starts in one place.
func adoptSingleRoot(tree *backupTree) bool {
	root := backupOldestRoot(*tree)
	changed := false
	for name, node := range tree.Nodes {
		if node.Parent == "" && name != root {
			node.Parent = root
			tree.Nodes[name] = node
			changed = true
		}
	}
	return changed
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
func reconcileTree(tree *backupTree, files []backupInfo) bool {
	changed := false
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
	sort.Slice(chronological, func(i, j int) bool { return chronological[i].CreatedAt.Before(chronological[j].CreatedAt) })

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
	return changed
}

// loadTreeLocked loads snapshot files and their tree index together, rebuilding the index when it is missing, stale or broken.
func (m *backupManager) loadTreeLocked(app string, files []backupInfo) (backupTree, bool, error) {
	tree, changed, err := m.readTreeLocked(app)
	if err != nil {
		return tree, false, err
	}
	return tree, reconcileTree(&tree, files) || changed, nil
}

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

// backupOldestLeaf returns the oldest node with no children outside the protected branch.
func backupOldestLeaf(tree backupTree, protected map[string]struct{}) string {
	children := map[string]int{}
	for _, node := range tree.Nodes {
		if node.Parent != "" {
			children[node.Parent]++
		}
	}
	oldest := ""
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
	return oldest
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
	changed := false
	for len(tree.Nodes) > keep {
		protected := backupPathNames(tree)
		if leaf := backupOldestLeaf(tree, protected); leaf != "" {
			if err := os.Remove(filepath.Join(m.appDir(app), leaf)); err != nil && !os.IsNotExist(err) {
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
		if err := os.Remove(filepath.Join(m.appDir(app), root)); err != nil && !os.IsNotExist(err) {
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
	if !changed {
		return nil
	}
	return m.saveTreeLocked(app, tree)
}
