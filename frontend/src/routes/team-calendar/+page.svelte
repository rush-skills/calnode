<script lang="ts">
	import { onMount } from 'svelte';
	import { base } from '$app/paths';
	import {
		api,
		type TeamCalendarItem,
		type TeamCalendarMember,
		type TeamCalendarResponse,
		type TeamCalendarShare,
		type TeamCalendarShareCreated
	} from '$lib/api';
	import { currentUser } from '$lib/stores';
	import { prefs } from '$lib/prefs';
	import { Button, buttonVariants } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import * as Dialog from '$lib/components/ui/dialog';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import * as Tooltip from '$lib/components/ui/tooltip';

	// ── calendar state ───────────────────────────────────────────────────────
	type View = 'week' | 'month';
	type Item = TeamCalendarItem & { s: Date; e: Date };

	const HOUR_PX = 48;
	let view = $state<View>('week');
	let anchor = $state(startOfDay(new Date()));
	let members = $state<TeamCalendarMember[]>([]);
	let items = $state<Item[]>([]);
	let hidden = $state<Record<string, boolean>>({});
	let loading = $state(true);
	let error = $state('');
	let selected = $state<Item | null>(null);
	let detailOpen = $state(false);
	let gridEl = $state<HTMLDivElement | null>(null);

	const isAdmin = $derived($currentUser?.is_admin ?? false);
	const weekStart = $derived($prefs.week_start ?? 1);
	const hour12 = $derived($prefs.time_format !== '24h');

	function startOfDay(d: Date) { const x = new Date(d); x.setHours(0, 0, 0, 0); return x; }
	function addDays(d: Date, n: number) { const x = new Date(d); x.setDate(x.getDate() + n); return x; }
	function startOfWeek(d: Date) { const x = startOfDay(d); return addDays(x, -((x.getDay() - weekStart + 7) % 7)); }
	function sameDay(a: Date, b: Date) { return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate(); }
	function ymd(d: Date) { return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`; }
	/** Local midnight of the calendar day named by an RFC3339 UTC-midnight string (all-day bounds). */
	function localDay(iso: string) { const [y, m, d] = iso.slice(0, 10).split('-').map(Number); return new Date(y, m - 1, d); }
	function fmtTime(d: Date) { return d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit', hour12 }); }
	function fmtDay(d: Date) { return d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' }); }

	/** The dates the grid shows: 7 days, or a 6-row month grid aligned to the week start. */
	const range = $derived.by(() => {
		if (view === 'week') { const s = startOfWeek(anchor); return { from: s, to: addDays(s, 7) }; }
		const s = startOfWeek(new Date(anchor.getFullYear(), anchor.getMonth(), 1));
		return { from: s, to: addDays(s, 42) };
	});
	// Fixed counts, not a millisecond diff: across a DST change the span is not a whole
	// number of 24h days and a truncated length would drop the grid's last column.
	const days = $derived(Array.from({ length: view === 'week' ? 7 : 42 }, (_, i) => addDays(range.from, i)));
	const rangeLabel = $derived(
		view === 'week'
			? `${fmtDay(range.from)} – ${fmtDay(addDays(range.to, -1))}`
			: anchor.toLocaleDateString(undefined, { month: 'long', year: 'numeric' })
	);
	const visible = $derived(items.filter((it) => !hidden[it.member_id]));
	const today = new Date();

	let loadSeq = 0;
	async function load() {
		const seq = ++loadSeq;
		loading = true;
		error = '';
		try {
			const res = await api.get<TeamCalendarResponse>(
				`/v1/team-calendar?from=${ymd(range.from)}&to=${ymd(addDays(range.to, -1))}`
			);
			if (seq !== loadSeq) return; // a later navigation superseded this fetch
			members = res.members;
			// An all-day event's bounds are UTC midnights naming calendar days; build them
			// as LOCAL midnights so the day survives the viewer's time zone. A timed event is
			// an instant and converts as-is.
			items = res.items.map((it) => ({
				...it,
				s: it.all_day ? localDay(it.start) : new Date(it.start),
				e: it.all_day ? localDay(it.end) : new Date(it.end)
			}));
		} catch (e: any) {
			if (seq === loadSeq) error = e.message;
		} finally {
			if (seq === loadSeq) loading = false;
		}
	}

	$effect(() => {
		// Re-fetch whenever the grid moves. Reading range here registers the dependency.
		void range.from.getTime();
		void range.to.getTime();
		load();
	});

	$effect(() => {
		if (!loading && view === 'week' && gridEl) gridEl.scrollTop = HOUR_PX * 7;
	});

	function step(n: number) {
		anchor = view === 'week' ? addDays(anchor, 7 * n) : new Date(anchor.getFullYear(), anchor.getMonth() + n, 1);
	}
	function member(id: string) { return members.find((m) => m.id === id); }
	function colorOf(it: Item) { return member(it.member_id)?.color ?? '#6b7280'; }
	function dayItems(day: Date, allDay?: boolean) {
		const next = addDays(day, 1);
		return visible.filter((it) => (allDay === undefined || it.all_day === allDay) && it.s < next && it.e > day);
	}
	function openDetail(it: Item) { selected = it; detailOpen = true; }

	type Placed = { it: Item; top: number; height: number; col: number; cols: number };
	/** Column layout for one day: overlapping events split the width, like any calendar app. */
	function layoutDay(day: Date): Placed[] {
		const mins = (t: Date) => Math.max(0, Math.min(1440, (t.getTime() - day.getTime()) / 60000));
		const sorted = dayItems(day, false).sort((a, b) => a.s.getTime() - b.s.getTime() || b.e.getTime() - a.e.getTime());
		const out: Placed[] = [];
		let cluster: Item[] = [];
		let clusterEnd: Date | null = null;
		const flush = () => {
			const colEnds: Date[] = [];
			const cols = new Map<Item, number>();
			for (const it of cluster) {
				let c = 0;
				while (colEnds[c] && colEnds[c] > it.s) c++;
				colEnds[c] = it.e;
				cols.set(it, c);
			}
			for (const it of cluster) {
				out.push({ it, col: cols.get(it)!, cols: colEnds.length, top: (mins(it.s) / 60) * HOUR_PX, height: ((mins(it.e) - mins(it.s)) / 60) * HOUR_PX });
			}
			cluster = [];
			clusterEnd = null;
		};
		for (const it of sorted) {
			if (clusterEnd && it.s >= clusterEnd) flush();
			cluster.push(it);
			if (!clusterEnd || it.e > clusterEnd) clusterEnd = it.e;
		}
		if (cluster.length) flush();
		return out;
	}
	function sourceLabel(it: Item) {
		if (it.kind === 'booking') return 'Calnode booking';
		if (it.source === 'google') return 'Google Calendar';
		if (it.source === 'microsoft') return 'Microsoft 365';
		return it.source;
	}

	// ── share links (admin) ──────────────────────────────────────────────────
	let shares = $state<TeamCalendarShare[]>([]);
	let sharesError = $state('');
	let shareName = $state('');
	let creating = $state(false);
	let created = $state<TeamCalendarShareCreated | null>(null);
	let revokeOpen = $state(false);
	let revokeTarget = $state<TeamCalendarShare | null>(null);
	let copied = $state(false);

	const snippet = $derived(
		created ? `<iframe src="${created.embed_url}" width="100%" height="700" style="border:0"></iframe>` : ''
	);

	async function loadShares() {
		try {
			shares = (await api.get<{ items: TeamCalendarShare[] }>('/v1/team-calendar/shares')).items;
		} catch (e: any) {
			sharesError = e.message;
		}
	}
	async function createShare() {
		if (!shareName.trim()) { sharesError = 'Give the share link a name.'; return; }
		creating = true;
		sharesError = '';
		copied = false;
		try {
			created = await api.post<TeamCalendarShareCreated>('/v1/team-calendar/shares', { name: shareName.trim() });
			shareName = '';
			await loadShares();
		} catch (e: any) {
			sharesError = e.message;
		} finally {
			creating = false;
		}
	}
	function askRevoke(s: TeamCalendarShare) { revokeTarget = s; revokeOpen = true; }
	async function doRevoke() {
		if (!revokeTarget) return;
		try {
			await api.del(`/v1/team-calendar/shares/${revokeTarget.id}`);
			if (created?.id === revokeTarget.id) created = null;
			await loadShares();
		} catch (e: any) {
			sharesError = e.message;
		}
	}
	function copySnippet() {
		// navigator.clipboard is undefined on plain-HTTP origins (a LAN self-host); the
		// snippet is still selectable in the <pre>, so just say so instead of throwing.
		if (!navigator.clipboard?.writeText) { sharesError = 'Clipboard unavailable on this origin — select the snippet and copy it.'; return; }
		navigator.clipboard.writeText(snippet).then(() => { copied = true; }).catch(() => {});
	}
	function fmtDate(iso: string) { return new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' }); }

	onMount(() => {
		const unsub = currentUser.subscribe((u) => { if (u?.is_admin) loadShares(); });
		return unsub;
	});
</script>

<svelte:head><title>Team calendar — Calnode</title></svelte:head>

<ConfirmDialog
	bind:open={revokeOpen}
	title="Revoke share link?"
	description={revokeTarget ? `Revoke "${revokeTarget.name}"? Any dashboard embedding it will stop showing the calendar immediately.` : ''}
	confirmText="Revoke"
	destructive
	onConfirm={doRevoke}
/>

<Dialog.Root bind:open={detailOpen}>
	<Dialog.Content class="max-w-md">
		{#if selected}
			<Dialog.Header>
				<Dialog.Title>
					<span class="mr-2 inline-block size-3 rounded-full align-middle" style="background:{colorOf(selected)}"></span>{selected.title}
				</Dialog.Title>
				<Dialog.Description>{sourceLabel(selected)}</Dialog.Description>
			</Dialog.Header>
			<dl class="grid grid-cols-[90px_1fr] gap-x-3 gap-y-2 text-sm">
				<dt class="text-muted-foreground">When</dt>
				<dd>{selected.all_day ? `${fmtDay(selected.s)} (all day)` : `${fmtDay(selected.s)}, ${fmtTime(selected.s)} – ${fmtTime(selected.e)}`}</dd>
				<dt class="text-muted-foreground">Member</dt>
				<dd>{member(selected.member_id)?.name ?? '—'}</dd>
				{#if selected.event_type_name}<dt class="text-muted-foreground">Event type</dt><dd>{selected.event_type_name}</dd>{/if}
				{#if selected.attendee_name}<dt class="text-muted-foreground">Attendee</dt><dd>{selected.attendee_name}</dd>{/if}
				{#if selected.location}<dt class="text-muted-foreground">Location</dt><dd class="break-all">{selected.location}</dd>{/if}
				<dt class="text-muted-foreground">Status</dt>
				<dd><Badge variant="outline">{selected.status}</Badge></dd>
			</dl>
			<Dialog.Footer class="mt-4">
				{#if selected.kind === 'booking'}
					<a href="{base}/bookings" class={buttonVariants({ variant: 'default' })}>Open in Bookings</a>
				{/if}
				<Button variant="outline" onclick={() => (detailOpen = false)}>Close</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>

<div class="mb-6 flex flex-wrap items-start justify-between gap-4">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">Team calendar</h1>
		<p class="mt-1 text-sm text-muted-foreground">Every member's meetings in one place, in your local time zone.</p>
	</div>
	<div class="flex shrink-0 items-center gap-2">
		<Button variant="outline" size="sm" onclick={() => step(-1)} aria-label="Previous">‹</Button>
		<Button variant="outline" size="sm" onclick={() => (anchor = startOfDay(new Date()))}>Today</Button>
		<Button variant="outline" size="sm" onclick={() => step(1)} aria-label="Next">›</Button>
		<span class="min-w-[180px] text-center text-sm font-medium">{rangeLabel}</span>
		<div class="ml-2 flex rounded-md border p-0.5" role="group" aria-label="View">
			<Button variant={view === 'week' ? 'secondary' : 'ghost'} size="sm" aria-pressed={view === 'week'} onclick={() => (view = 'week')}>Week</Button>
			<Button variant={view === 'month' ? 'secondary' : 'ghost'} size="sm" aria-pressed={view === 'month'} onclick={() => (view = 'month')}>Month</Button>
		</div>
	</div>
</div>

{#if members.length}
	<div class="mb-4 flex flex-wrap gap-2" aria-label="Members">
		{#each members as m (m.id)}
			<Button
				variant={hidden[m.id] ? 'ghost' : 'outline'}
				size="sm"
				class="h-7 rounded-full px-2.5 {hidden[m.id] ? 'opacity-50' : ''}"
				aria-pressed={!hidden[m.id]}
				onclick={() => (hidden = { ...hidden, [m.id]: !hidden[m.id] })}
			>
				<span class="size-2.5 rounded-full" style="background:{m.color}"></span>
				{m.name}
			</Button>
		{/each}
	</div>
{/if}

{#if error}<p class="mb-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</p>{/if}

<!-- The grid below is hand-built: shadcn-svelte has no week/month scheduling grid (its
     Calendar is a date picker). Everything around it stays shadcn. See docs/features/team-calendar.md. -->
<div class="relative overflow-hidden rounded-lg border bg-card">
	{#if loading}
		<div class="absolute inset-0 z-20 flex items-center justify-center bg-card/60 text-sm text-muted-foreground">Loading…</div>
	{/if}
	{#if view === 'week'}
		<div class="max-h-[70vh] overflow-auto" bind:this={gridEl}>
			<div class="grid min-w-[700px]" style="grid-template-columns: 56px repeat(7, minmax(0, 1fr))">
				<div class="sticky top-0 z-10 border-b bg-card"></div>
				{#each days as d (d.getTime())}
					<div class="sticky top-0 z-10 border-b border-l bg-card px-1 py-1.5 text-center text-xs {sameDay(d, today) ? 'text-primary' : 'text-muted-foreground'}">
						{d.toLocaleDateString(undefined, { weekday: 'short' })}
						<div class="text-base font-semibold text-foreground {sameDay(d, today) ? 'text-primary' : ''}">{d.getDate()}</div>
					</div>
				{/each}
				<div class="border-b"></div>
				{#each days as d (d.getTime())}
					<div class="flex min-h-6 flex-col gap-0.5 border-b border-l p-0.5">
						{#each dayItems(d, true) as it (it.id)}
							<button type="button" class="truncate rounded px-1 text-left text-[11px] font-semibold text-white" style="background:{colorOf(it)}" onclick={() => openDetail(it)} title={it.title}>{it.title}</button>
						{/each}
					</div>
				{/each}
				<div class="relative">
					{#each Array.from({ length: 24 }, (_, h) => h) as h}
						<div class="-translate-y-2 pr-1 text-right text-[10px] text-muted-foreground" style="height:{HOUR_PX}px">{h ? fmtTime(new Date(2000, 0, 1, h)) : ''}</div>
					{/each}
				</div>
				{#each days as d (d.getTime())}
					<div class="relative border-l" style="height:{HOUR_PX * 24}px; background-image: linear-gradient(to bottom, var(--border) 1px, transparent 1px); background-size: 100% {HOUR_PX}px">
						{#each layoutDay(d) as p (p.it.id)}
							<button
								type="button"
								class="absolute overflow-hidden rounded border px-1 py-0.5 text-left text-[11px] leading-tight {p.it.kind === 'external' ? 'text-foreground' : 'text-white'}"
								style="top:{p.top}px; height:{Math.max(p.height, 18)}px; left:calc({(p.col * 100) / p.cols}% + 2px); width:calc({100 / p.cols}% - 4px); {p.it.kind === 'external' ? `background: color-mix(in srgb, ${colorOf(p.it)} 18%, white); border-color:${colorOf(p.it)}` : `background:${colorOf(p.it)}; border-color: rgba(255,255,255,.6)`}"
								onclick={() => openDetail(p.it)}
								title={p.it.title}
							>
								<div class="truncate font-semibold">{p.it.title}</div>
								<div class="truncate opacity-80">{fmtTime(p.it.s)} · {member(p.it.member_id)?.name ?? ''}</div>
							</button>
						{/each}
					</div>
				{/each}
			</div>
		</div>
	{:else}
		<div class="overflow-auto">
			<div class="grid min-w-[640px] grid-cols-7">
				{#each days.slice(0, 7) as d (d.getTime())}
					<div class="border-b px-1 py-1.5 text-center text-xs text-muted-foreground">{d.toLocaleDateString(undefined, { weekday: 'short' })}</div>
				{/each}
				{#each days as d (d.getTime())}
					{@const list = dayItems(d)}
					<div class="min-h-24 border-b border-r p-1 {d.getMonth() !== anchor.getMonth() ? 'bg-muted/40 text-muted-foreground' : ''}">
						<div class="mb-0.5 text-xs font-semibold {sameDay(d, today) ? 'text-primary' : ''}">{d.getDate()}</div>
						{#each list.slice(0, 4) as it (it.id)}
							<button
								type="button"
								class="mb-0.5 block w-full truncate rounded border px-1 text-left text-[11px] {it.kind === 'external' ? 'text-foreground' : 'font-semibold text-white'}"
								style={it.kind === 'external' ? `background: color-mix(in srgb, ${colorOf(it)} 18%, white); border-color:${colorOf(it)}` : `background:${colorOf(it)}; border-color:${colorOf(it)}`}
								onclick={() => openDetail(it)}
								title={it.title}
							>{it.all_day ? '' : fmtTime(it.s) + ' '}{it.title}</button>
						{/each}
						{#if list.length > 4}
							<button type="button" class="text-[11px] text-muted-foreground hover:text-foreground" onclick={() => { anchor = d; view = 'week'; }}>+{list.length - 4} more</button>
						{/if}
					</div>
				{/each}
			</div>
		</div>
	{/if}
</div>

{#if isAdmin}
	<section class="mt-8 rounded-lg border bg-card p-6">
		<h2 class="text-base font-semibold">Share / embed</h2>
		<p class="mt-1 text-sm text-muted-foreground">
			Create a share link to embed this calendar in an external dashboard. Anyone with the link can see every member's
			meetings, including attendee names — treat it like a password and revoke it when it is no longer needed.
		</p>

		{#if sharesError}<p class="mt-3 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{sharesError}</p>{/if}

		<div class="mt-4 flex flex-wrap items-end gap-3">
			<div class="w-full max-w-xs space-y-1.5">
				<Label for="share-name">Share link name</Label>
				<Input id="share-name" bind:value={shareName} placeholder="e.g. Office lobby screen" onkeydown={(e) => { if (e.key === 'Enter') createShare(); }} />
			</div>
			<Button onclick={createShare} disabled={creating}>{creating ? 'Creating…' : 'Create share link'}</Button>
		</div>

		{#if created}
			<div class="mt-4 rounded-lg border border-green-200 bg-green-50 p-4">
				<p class="mb-2 text-sm font-medium text-green-800">Share link created — copy the snippet now. The token will not be shown again.</p>
				<pre class="mb-3 overflow-x-auto rounded-md border bg-white px-3 py-2 font-mono text-xs text-foreground whitespace-pre-wrap break-all">{snippet}</pre>
				<Button variant="outline" size="sm" onclick={copySnippet}>{copied ? 'Copied' : 'Copy snippet'}</Button>
			</div>
		{/if}

		{#if shares.length}
			<div class="mt-5 overflow-hidden rounded-lg border">
				<table class="w-full text-sm">
					<thead>
						<tr class="border-b">
							<th class="px-4 py-2.5 text-left text-xs font-medium text-muted-foreground">Name</th>
							<th class="px-4 py-2.5 text-left text-xs font-medium text-muted-foreground">Created</th>
							<th class="px-4 py-2.5 text-left text-xs font-medium text-muted-foreground">Status</th>
							<th class="px-4 py-2.5"></th>
						</tr>
					</thead>
					<tbody class="divide-y">
						<Tooltip.Provider>
							{#each shares as s (s.id)}
								<tr class="transition-colors hover:bg-muted/30 {s.revoked_at ? 'text-muted-foreground' : ''}">
									<td class="px-4 py-2.5 font-medium">{s.name}</td>
									<td class="px-4 py-2.5 text-muted-foreground">{fmtDate(s.created_at)}{s.created_by ? ` · ${s.created_by}` : ''}</td>
									<td class="px-4 py-2.5">
										{#if s.revoked_at}<Badge variant="outline">Revoked {fmtDate(s.revoked_at)}</Badge>{:else}<Badge variant="secondary">Active</Badge>{/if}
									</td>
									<td class="px-4 py-2.5 text-right">
										{#if !s.revoked_at}
											<Tooltip.Root>
												<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => askRevoke(s)} aria-label="Revoke share link">
													<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4h6v2"/></svg>
												</Tooltip.Trigger>
												<Tooltip.Content>Revoke share link</Tooltip.Content>
											</Tooltip.Root>
										{/if}
									</td>
								</tr>
							{/each}
						</Tooltip.Provider>
					</tbody>
				</table>
			</div>
		{/if}
	</section>
{/if}
