import type { BackupInfo, BackupKind } from "./api"

export interface BackupGraphNode {
	/** The node the live database follows. */
	current: boolean
	id: string
	label: string
	/** An ancestor of the current node. */
	onPath: boolean
	/** A direct child of the current node; taking a snapshot here would continue from it. */
	redoable: boolean
	/** Whether the label fits beside the node without colliding with the next one. */
	showLabel: boolean
	/** Full label for the tooltip and accessible name. */
	title: string
	/** Centre position in layout pixels. */
	x: number
	y: number
}

export interface BackupGraphEdge {
	/** SVG path data for the curved connector. */
	d: string
	from: string
	/** Whether the edge belongs to the current lineage. */
	onPath: boolean
	to: string
}

export interface BackupGraph {
	edges: BackupGraphEdge[]
	height: number
	nodes: BackupGraphNode[]
	width: number
}

/** Diameter of a graph node; shared with the view so label fitting matches what's drawn. */
export const BACKUP_NODE_SIZE = 16
// Spacing of the graph, in layout pixels.
const COLUMN_GAP = 88
const ROW_GAP = 56
// Gap and rough per-character width used to decide whether a node's label fits beside it.
const LABEL_GAP = 6
const LABEL_CHAR_WIDTH = 6

const nodeTimeFormat = new Intl.DateTimeFormat(undefined, {
	hour: "2-digit",
	hour12: false,
	minute: "2-digit",
	second: "2-digit",
})

const nodeFullTimeFormat = new Intl.DateTimeFormat(undefined, {
	dateStyle: "medium",
	timeStyle: "medium",
})

/**
 * A cubic Bézier connector between two node centres. Both control points sit at the vertical midpoint, so the curve leaves the parent and enters the child straight down and bends smoothly across.
 */
const edgePath = (x1: number, y1: number, x2: number, y2: number): string => {
	const mid = (y1 + y2) / 2
	return `M ${x1} ${y1} C ${x1} ${mid} ${x2} ${mid} ${x2} ${y2}`
}

const backupKindLabels: { [_: string]: string } = {
	"": "Backup",
	auto: "Automatic",
	manual: "Manual",
}

export function backupKindLabel(kind: BackupKind): string {
	return backupKindLabels[kind] ?? "Backup"
}

/** Short label for a graph node; the full date is in the tooltip and the details below the tree. */
export function backupNodeLabel(backup: BackupInfo): string {
	const date = new Date(backup.createdAt)
	return Number.isNaN(date.getTime()) ? backup.name : nodeTimeFormat.format(date)
}

/** Full label for tooltips and accessible names. */
export function backupTitle(backup: BackupInfo): string {
	const date = new Date(backup.createdAt)
	const when = Number.isNaN(date.getTime()) ? backup.name : nodeFullTimeFormat.format(date)
	return `${when} · ${backupKindLabel(backup.kind)}`
}

/** Lay the ancestry out as a tidy tree: depth runs down, leaves spread across, and each parent centres over its children. */
export function backupGraph(backups: BackupInfo[], current: string): BackupGraph {
	const empty: BackupGraph = { edges: [], height: 0, nodes: [], width: 0 }
	if (backups.length === 0) return empty

	const byName = new Map(backups.map(backup => [backup.name, backup]))
	const ordered = backups.toSorted((a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt))
	const root = ordered.find(backup => backup.parent === "" || !byName.has(backup.parent)) ?? ordered[0]
	if (!root) return empty

	// Children stay in creation order, so the layout depends only on the tree and selecting a node never moves it.
	const children = new Map<string, BackupInfo[]>()
	for (const backup of ordered) {
		if (backup.name === root.name) continue
		const parent =
			backup.parent && backup.parent !== backup.name && byName.has(backup.parent) ? backup.parent : root.name
		const siblings = children.get(parent) ?? []
		siblings.push(backup)
		children.set(parent, siblings)
	}
	const childNodes = (backup: BackupInfo): BackupInfo[] => children.get(backup.name) ?? []

	const onPath = new Set<string>()
	for (let id = current; id && byName.has(id) && !onPath.has(id); ) {
		onPath.add(id)
		id = byName.get(id)?.parent ?? ""
	}
	const redoable = new Set(childNodes(byName.get(current) ?? root).map(child => child.name))

	const positions = new Map<string, { x: number; y: number }>()
	let nextColumn = 0
	const place = (backup: BackupInfo, depth: number): number => {
		const kids = childNodes(backup)
		let column: number
		if (kids.length === 0) column = nextColumn++
		else {
			const columns = kids.map(child => place(child, depth + 1))
			column = ((columns[0] ?? 0) + (columns.at(-1) ?? 0)) / 2
		}
		positions.set(backup.name, { x: column, y: depth })
		return column
	}
	place(root, 0)

	const graphNodes: BackupGraphNode[] = []
	const edges: BackupGraphEdge[] = []
	const walk = (backup: BackupInfo): void => {
		const position = positions.get(backup.name)
		if (position)
			graphNodes.push({
				current: backup.name === current,
				id: backup.name,
				label: backupNodeLabel(backup),
				onPath: onPath.has(backup.name),
				redoable: redoable.has(backup.name),
				showLabel: false,
				title: backupTitle(backup),
				x: (position.x + 0.5) * COLUMN_GAP,
				y: (position.y + 0.5) * ROW_GAP,
			})

		for (const child of childNodes(backup)) {
			const from = positions.get(backup.name)
			const to = positions.get(child.name)
			if (from && to)
				edges.push({
					d: edgePath(
						(from.x + 0.5) * COLUMN_GAP,
						(from.y + 0.5) * ROW_GAP,
						(to.x + 0.5) * COLUMN_GAP,
						(to.y + 0.5) * ROW_GAP,
					),
					from: backup.name,
					onPath: onPath.has(backup.name) && onPath.has(child.name),
					to: child.name,
				})

			walk(child)
		}
	}
	walk(root)

	let maxColumn = 0
	let maxDepth = 0
	for (const position of positions.values()) {
		maxColumn = Math.max(maxColumn, position.x)
		maxDepth = Math.max(maxDepth, position.y)
	}

	// Only show a label when it fits before the next node on its row, so labels never collide. The last node on a row has open space to its right.
	const rows = new Map<number, BackupGraphNode[]>()
	for (const node of graphNodes) {
		const row = rows.get(node.y)
		if (row) row.push(node)
		else rows.set(node.y, [node])
	}
	for (const row of rows.values()) {
		const sorted = row.toSorted((a, b) => a.x - b.x)
		for (const [index, node] of sorted.entries()) {
			const next = sorted[index + 1]
			if (!next) {
				node.showLabel = true
				continue
			}
			const available = next.x - node.x - BACKUP_NODE_SIZE - LABEL_GAP
			node.showLabel = available >= node.label.length * LABEL_CHAR_WIDTH
		}
	}

	return {
		edges,
		height: (maxDepth + 1) * ROW_GAP,
		nodes: graphNodes,
		width: (maxColumn + 1) * COLUMN_GAP,
	}
}
