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
	import { t, tr, localeTag } from '$lib/i18n';
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
	function fmtTime(d: Date) { return d.toLocaleTimeString(localeTag(), { hour: 'numeric', minute: '2-digit', hour12 }); }
	function fmtDay(d: Date) { return d.toLocaleDateString(localeTag(), { weekday: 'short', month: 'short', day: 'numeric' }); }

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
			: anchor.toLocaleDateString(localeTag(), { month: 'long', year: 'numeric' })
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
		if (it.kind === 'booking') return tr('teamCalendar.source.booking');
		if (it.source === 'google') return tr('teamCalendar.source.google');
		if (it.source === 'microsoft') return tr('teamCalendar.source.microsoft');
		return it.source;
	}
	function statusLabel(status: string) {
		if (status === 'confirmed') return tr('bookings.status.confirmed');
		if (status === 'cancelled') return tr('bookings.status.cancelled');
		if (status === 'rescheduled') return tr('bookings.filter.status.rescheduled');
		return status;
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
		if (!shareName.trim()) { sharesError = tr('teamCalendar.share.error.nameRequired'); return; }
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
		if (!navigator.clipboard?.writeText) { sharesError = tr('teamCalendar.share.error.clipboardUnavailable'); return; }
		navigator.clipboard.writeText(snippet).then(() => { copied = true; }).catch(() => {});
	}
	function fmtDate(iso: string) { return new Date(iso).toLocaleDateString(localeTag(), { dateStyle: 'medium' }); }

	onMount(() => {
		const unsub = currentUser.subscribe((u) => { if (u?.is_admin) loadShares(); });
		return unsub;
	});
</script>

<svelte:head><title>{$t('teamCalendar.headTitle')}</title></svelte:head>

<ConfirmDialog
	bind:open={revokeOpen}
	title={$t('teamCalendar.share.confirmRevoke.title')}
	description={revokeTarget ? $t('teamCalendar.share.confirmRevoke.description', { name: revokeTarget.name }) : ''}
	confirmText={$t('teamCalendar.share.confirmRevoke.confirm')}
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
				<dt class="text-muted-foreground">{$t('teamCalendar.detail.when')}</dt>
				<dd>{selected.all_day ? $t('teamCalendar.detail.allDay', { date: fmtDay(selected.s) }) : $t('teamCalendar.detail.timeRange', { date: fmtDay(selected.s), start: fmtTime(selected.s), end: fmtTime(selected.e) })}</dd>
				<dt class="text-muted-foreground">{$t('teamCalendar.detail.member')}</dt>
				<dd>{member(selected.member_id)?.name ?? $t('common.emDash')}</dd>
				{#if selected.event_type_name}<dt class="text-muted-foreground">{$t('teamCalendar.detail.eventType')}</dt><dd>{selected.event_type_name}</dd>{/if}
				{#if selected.attendee_name}<dt class="text-muted-foreground">{$t('teamCalendar.detail.attendee')}</dt><dd>{selected.attendee_name}</dd>{/if}
				{#if selected.location}<dt class="text-muted-foreground">{$t('teamCalendar.detail.location')}</dt><dd class="break-all">{selected.location}</dd>{/if}
				<dt class="text-muted-foreground">{$t('teamCalendar.detail.status')}</dt>
				<dd><Badge variant="outline">{statusLabel(selected.status)}</Badge></dd>
			</dl>
			<Dialog.Footer class="mt-4">
				<!-- Bookings only lists the viewer's own meetings (admins see everyone's), so the
				     link is offered only when it would actually show this item. -->
				{#if selected.kind === 'booking' && (isAdmin || selected.member_id === $currentUser?.id)}
					<a href="{base}/bookings" class={buttonVariants({ variant: 'default' })}>{$t('teamCalendar.detail.openInBookings')}</a>
				{/if}
				<Button variant="outline" onclick={() => (detailOpen = false)}>{$t('common.close')}</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>

<div class="mb-6 flex flex-wrap items-start justify-between gap-4">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">{$t('teamCalendar.title')}</h1>
		<p class="mt-1 text-sm text-muted-foreground">{$t('teamCalendar.subtitle')}</p>
	</div>
	<div class="flex shrink-0 items-center gap-2">
		<Button variant="outline" size="sm" onclick={() => step(-1)} aria-label={$t('common.previous')}>‹</Button>
		<Button variant="outline" size="sm" onclick={() => (anchor = startOfDay(new Date()))}>{$t('teamCalendar.today')}</Button>
		<Button variant="outline" size="sm" onclick={() => step(1)} aria-label={$t('common.next')}>›</Button>
		<span class="min-w-[180px] text-center text-sm font-medium">{rangeLabel}</span>
		<div class="ml-2 flex rounded-md border p-0.5" role="group" aria-label={$t('teamCalendar.viewAria')}>
			<Button variant={view === 'week' ? 'secondary' : 'ghost'} size="sm" aria-pressed={view === 'week'} onclick={() => (view = 'week')}>{$t('teamCalendar.view.week')}</Button>
			<Button variant={view === 'month' ? 'secondary' : 'ghost'} size="sm" aria-pressed={view === 'month'} onclick={() => (view = 'month')}>{$t('teamCalendar.view.month')}</Button>
		</div>
	</div>
</div>

{#if members.length}
	<div class="mb-4 flex flex-wrap gap-2" aria-label={$t('teamCalendar.membersAria')}>
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
		<div class="absolute inset-0 z-20 flex items-center justify-center bg-card/60 text-sm text-muted-foreground">{$t('common.loading')}</div>
	{/if}
	{#if view === 'week'}
		<div class="max-h-[70vh] overflow-auto" bind:this={gridEl}>
			<!-- Inline min-width: app.css has an unlayered `* { min-width: 0 }` that beats Tailwind min-w utilities. -->
			<div class="grid" style="min-width: 700px; grid-template-columns: 56px repeat(7, minmax(0, 1fr))">
				<div class="sticky top-0 z-10 border-b bg-card"></div>
				{#each days as d (d.getTime())}
					<div class="sticky top-0 z-10 border-b border-l bg-card px-1 py-1.5 text-center text-xs {sameDay(d, today) ? 'text-primary' : 'text-muted-foreground'}">
						{d.toLocaleDateString(localeTag(), { weekday: 'short' })}
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
			<div class="grid grid-cols-7" style="min-width: 640px">
				{#each days.slice(0, 7) as d (d.getTime())}
					<div class="border-b px-1 py-1.5 text-center text-xs text-muted-foreground">{d.toLocaleDateString(localeTag(), { weekday: 'short' })}</div>
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
							<button type="button" class="text-[11px] text-muted-foreground hover:text-foreground" onclick={() => { anchor = d; view = 'week'; }}>{$t('teamCalendar.more', { count: list.length - 4 })}</button>
						{/if}
					</div>
				{/each}
			</div>
		</div>
	{/if}
</div>

{#if isAdmin}
	<section class="mt-8 rounded-lg border bg-card p-6">
		<h2 class="text-base font-semibold">{$t('teamCalendar.share.title')}</h2>
		<p class="mt-1 text-sm text-muted-foreground">
			{$t('teamCalendar.share.description')}
		</p>

		{#if sharesError}<p class="mt-3 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{sharesError}</p>{/if}

		<div class="mt-4 flex flex-wrap items-end gap-3">
			<div class="w-full max-w-xs space-y-1.5">
				<Label for="share-name">{$t('teamCalendar.share.nameLabel')}</Label>
				<Input id="share-name" bind:value={shareName} placeholder={$t('teamCalendar.share.namePlaceholder')} onkeydown={(e) => { if (e.key === 'Enter') createShare(); }} />
			</div>
			<Button onclick={createShare} disabled={creating}>{creating ? $t('teamCalendar.share.creating') : $t('teamCalendar.share.create')}</Button>
		</div>

		{#if created}
			<div class="mt-4 rounded-lg border border-green-200 bg-green-50 p-4">
				<p class="mb-2 text-sm font-medium text-green-800">{$t('teamCalendar.share.createdNotice')}</p>
				<pre class="mb-3 overflow-x-auto rounded-md border bg-white px-3 py-2 font-mono text-xs text-foreground whitespace-pre-wrap break-all">{snippet}</pre>
				<Button variant="outline" size="sm" onclick={copySnippet}>{copied ? $t('common.copiedShort') : $t('teamCalendar.share.copySnippet')}</Button>
			</div>
		{/if}

		{#if shares.length}
			<div class="mt-5 overflow-hidden rounded-lg border">
				<table class="w-full text-sm">
					<thead>
						<tr class="border-b">
							<th class="px-4 py-2.5 text-left text-xs font-medium text-muted-foreground">{$t('teamCalendar.share.table.name')}</th>
							<th class="px-4 py-2.5 text-left text-xs font-medium text-muted-foreground">{$t('teamCalendar.share.table.created')}</th>
							<th class="px-4 py-2.5 text-left text-xs font-medium text-muted-foreground">{$t('teamCalendar.share.table.status')}</th>
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
										{#if s.revoked_at}<Badge variant="outline">{$t('teamCalendar.share.revokedOn', { date: fmtDate(s.revoked_at) })}</Badge>{:else}<Badge variant="secondary">{$t('common.active')}</Badge>{/if}
									</td>
									<td class="px-4 py-2.5 text-right">
										{#if !s.revoked_at}
											<Tooltip.Root>
												<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => askRevoke(s)} aria-label={$t('teamCalendar.share.revokeTooltip')}>
													<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4h6v2"/></svg>
												</Tooltip.Trigger>
												<Tooltip.Content>{$t('teamCalendar.share.revokeTooltip')}</Tooltip.Content>
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
