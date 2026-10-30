<script lang="ts">
import type { DatabaseRows, StorageChange } from "./api"
import { formatChangeParams, formatDatabaseValue, formatRowValue, formatTime, rowOpClass } from "./format"

interface Props {
	changes: StorageChange[]
	changesError: string
	changesLoading: boolean
	error: string
	exists: boolean
	limit: number
	loading: boolean
	offset: number
	onchangelimit(event: Event): void
	onpage(direction: -1 | 1): void
	onreload(): void
	onselecttable(event: Event): void
	rows: DatabaseRows | null
	rowsLoading: boolean
	table: string | null
	tables: string[]
}

let {
	changes,
	changesError,
	changesLoading,
	error,
	exists,
	limit,
	loading,
	offset,
	onchangelimit,
	onpage,
	onreload,
	onselecttable,
	rows,
	rowsLoading,
	table,
	tables,
}: Props = $props()
</script>

<div
	aria-labelledby="app-tab-database"
	class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4"
	id="app-panel-database"
	role="tabpanel"
>
	<div class="flex flex-wrap items-center justify-between gap-2">
		<div class="flex items-center gap-2">
			<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="database-title">Database</h3>
			<span class="text-xs text-neutral-500">
				{tables.length}
				{tables.length === 1 ? "table" : "tables"}
			</span>
		</div>
		<button class="btn" disabled={loading} onclick={onreload} type="button">
			{loading ? "Refreshing…" : "Refresh"}
		</button>
	</div>

	{#if error}
		<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
			{error}
		</p>
	{/if}

	{#if loading}
		<p class="text-sm text-neutral-500">Loading database…</p>
	{:else if !exists}
		<p class="text-sm text-neutral-500">No database yet. One is created the first time the app uses storage.</p>
	{:else if tables.length === 0}
		<p class="text-sm text-neutral-500">The database has no tables yet.</p>
	{:else}
		<div class="flex flex-wrap items-end gap-3">
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase"> Table </span>
				<select aria-label="Table" class="field" onchange={onselecttable} value={table ?? ""}>
					{#each tables as table (table)}
						<option value={table}>{table}</option>
					{/each}
				</select>
			</label>
			<label class="flex flex-col gap-1">
				<span class="text-[10px] tracking-wide text-neutral-500 uppercase"> Rows </span>
				<select aria-label="Rows per page" class="field" onchange={onchangelimit} value={limit}>
					{#each [25, 50, 100] as limit (limit)}
						<option value={limit}>{limit}</option>
					{/each}
				</select>
			</label>
			<span class="pb-1.5 text-xs text-neutral-500 tabular-nums">
				{#if rows && rows.rows.length > 0}
					{rows.offset + 1}-{rows.offset + rows.rows.length}
					of {rows.total}
				{:else if rows}
					0 of {rows.total}
				{:else}
					…
				{/if}
			</span>
			<span class="flex-1"></span>
			<button class="btn" disabled={rowsLoading || offset === 0} onclick={(): void => onpage(-1)} type="button">
				Previous
			</button>
			<button
				class="btn"
				disabled={rowsLoading || !rows?.truncated || (rows?.rows.length ?? 0) === 0}
				onclick={(): void => onpage(1)}
				type="button"
			>
				Next
			</button>
		</div>

		{#if rowsLoading}
			<p class="text-sm text-neutral-500">Loading rows…</p>
		{:else if rows}
			<div class="overflow-x-auto border border-line-soft">
				<table class="w-full border-collapse text-left text-xs">
					<thead class="bg-surface-raised text-[10px] tracking-wide text-neutral-500 uppercase">
						<tr>
							{#each rows.columns as column (column)}
								<th class="px-2 py-1 font-normal whitespace-nowrap">
									{column}
								</th>
							{/each}
						</tr>
					</thead>
					<tbody>
						{#if rows.rows.length === 0}
							<tr>
								<td class="px-2 py-2 text-neutral-500" colspan={Math.max(rows.columns.length, 1)}>
									No rows.
								</td>
							</tr>
						{:else}
							{#each rows.rows as row, rowIndex (rowIndex)}
								<tr class="border-t border-line-soft">
									{#each row as cell, cellIndex (cellIndex)}
										{const text = formatDatabaseValue(cell)}
										<td class="max-w-80 truncate px-2 py-1 font-mono text-neutral-300" title={text}>
											{text}
										</td>
									{/each}
								</tr>
							{/each}
						{/if}
					</tbody>
				</table>
			</div>
		{/if}
	{/if}

	<section aria-labelledby="changes-title" class="flex flex-col gap-2 border border-line-soft bg-surface p-2">
		<div class="flex items-center justify-between gap-2">
			<h3 class="text-xs font-semibold tracking-wide text-neutral-400 uppercase" id="changes-title">
				Change log
			</h3>
			<span class="text-xs text-neutral-500">{changes.length} recent</span>
		</div>

		{#if changesError}
			<p class="border border-red-500/80 bg-red-500/10 px-2 py-1 text-sm text-red-400" role="alert">
				{changesError}
			</p>
		{/if}

		{#if changesLoading}
			<p class="text-sm text-neutral-500">Loading changes…</p>
		{:else if changes.length === 0}
			<p class="text-sm text-neutral-500">No changes logged yet.</p>
		{:else}
			<ul class="flex flex-col">
				{#each changes as change, index (index)}
					<li class="flex flex-col gap-1 border-t border-line-soft py-2 text-xs first:border-t-0 first:pt-0">
						<div class="flex flex-wrap items-center justify-between gap-2 text-neutral-500">
							<time datetime={change.time}>{formatTime(change.time)}</time>
							<span class="tabular-nums">
								{change.rowsAffected}
								{change.rowsAffected === 1 ? "row" : "rows"}
							</span>
						</div>
						<code class="whitespace-pre-wrap wrap-break-word text-neutral-200">
							{change.sql}
						</code>
						{#if change.params.length > 0}
							<code class="whitespace-pre-wrap wrap-break-word text-neutral-500">
								{formatChangeParams(change.params)}
							</code>
						{/if}
						{#if change.rows && change.rows.length > 0}
							<div class="flex flex-col gap-1.5 pt-0.5">
								{#each change.rows as row, rowIndex (rowIndex)}
									<div class="border border-line-soft bg-black/30 p-1.5">
										<div
											class="flex flex-wrap items-center gap-2 pb-1 text-[10px] tracking-wide uppercase"
										>
											<span class={rowOpClass(row.op)}>{row.op}</span>
											<span class="normal-case text-neutral-400">
												{row.table}
											</span>
											{#if row.rowId !== undefined}
												<span class="normal-case text-neutral-500 tabular-nums">
													row {row.rowId}
												</span>
											{/if}
										</div>
										<dl class="grid grid-cols-1 gap-x-3 gap-y-1 text-xs sm:grid-cols-2">
											{#each row.columns as column, columnIndex (column)}
												<div class="flex min-w-0 items-baseline gap-1.5">
													<dt class="shrink-0 text-neutral-500">
														{column}
													</dt>
													<dd class="min-w-0 wrap-break-word text-neutral-300">
														{#if row.op === "update"}
															<span class="text-neutral-500">
																{formatRowValue(row.old, columnIndex)}
															</span>
															<span class="text-neutral-600"> → </span>
														{/if}
														{#if row.op === "delete"}
															<span>
																{formatRowValue(row.old, columnIndex)}
															</span>
														{:else}
															<span>
																{formatRowValue(row.new, columnIndex)}
															</span>
														{/if}
													</dd>
												</div>
											{/each}
										</dl>
									</div>
								{/each}
								{#if change.rowsTruncated}
									<p class="text-[10px] text-neutral-500">Some row details were omitted.</p>
								{/if}
							</div>
						{:else if change.rowsTruncated}
							<p class="text-[10px] text-neutral-500">
								Row details were omitted because the change was too large.
							</p>
						{/if}
					</li>
				{/each}
			</ul>
		{/if}
	</section>
</div>
