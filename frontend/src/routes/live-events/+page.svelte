<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import { api, type LiveEvent, type LiveEventInput, type TeamMember } from '$lib/api';
	import { currentUser } from '$lib/stores';
	import { Button, buttonVariants } from '$lib/components/ui/button';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import * as Dialog from '$lib/components/ui/dialog';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Textarea } from '$lib/components/ui/textarea';
	import { Badge } from '$lib/components/ui/badge';
	import { Checkbox } from '$lib/components/ui/checkbox';
	import { Switch } from '$lib/components/ui/switch';
	import * as Select from '$lib/components/ui/select';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { DatePicker } from '$lib/components/ui/date-picker';
	import { toast } from 'svelte-sonner';

	const KIND_LABELS: Record<LiveEvent['kind'], string> = { office_hours: 'Office hours', event: 'Event' };
	const POLL_MS = 30_000;

	let items = $state<LiveEvent[]>([]);
	let loading = $state(true);
	let showEnded = $state(false);
	let members = $state<TeamMember[]>([]);
	let pollTimer: ReturnType<typeof setInterval> | null = null;

	// ---- Create dialog ----
	let createOpen = $state(false);
	let creating = $state(false);
	let createError = $state('');
	const emptyForm = () => ({
		title: '',
		description: '',
		kind: 'office_hours' as LiveEvent['kind'],
		start_date: '',
		start_time: '',
		end_date: '',
		end_time: '',
		start_now: false,
		host_user_id: '',
		join_url: '',
		auto_start: true,
		auto_end: true
	});
	let form = $state(emptyForm());

	// ---- Confirm dialog (End / Cancel) ----
	let confirmOpen = $state(false);
	let confirmTitle = $state('');
	let confirmDescription = $state('');
	let confirmActionText = $state('');
	let confirmDestructive = $state(false);
	let pendingAction: (() => void) | null = null;

	const visible = $derived(
		showEnded ? items : items.filter((e) => e.status === 'scheduled' || e.status === 'live')
	);
	const liveCount = $derived(items.filter((e) => e.status === 'live').length);
	const origin = $derived(typeof window !== 'undefined' ? window.location.origin : '');
	const iframeSnippet = $derived(
		`<iframe src="${origin}/live" title="Live now" width="100%" height="320" style="border:0;border-radius:12px" loading="lazy"></iframe>`
	);
	const widgetSnippet = $derived(
		`<script src="${origin}/live-widget.js" async><\/script>\n<calnode-live data-kind="office_hours" data-poll="30"></calnode-live>`
	);

	async function load(quiet = false) {
		try {
			const res = await api.get<{ live_events: LiveEvent[] }>('/v1/live-events?limit=100');
			items = res.live_events ?? [];
		} catch (e: any) {
			if (!quiet) toast.error(e.message || 'Could not load live events');
		} finally {
			loading = false;
		}
	}

	onMount(async () => {
		await load();
		pollTimer = setInterval(() => load(true), POLL_MS);
		if ($currentUser?.is_admin) {
			try {
				members = ((await api.get<TeamMember[]>('/v1/users')) ?? []).filter((m) => !m.archived);
			} catch {
				// Host picker simply shows only "me".
			}
		}
	});
	onDestroy(() => {
		if (pollTimer) clearInterval(pollTimer);
	});

	// Date + time in the viewer's zone → RFC3339 UTC. The server stores and compares UTC only.
	function toISO(date: string, time: string): string | null {
		if (!date) return null;
		const d = new Date(`${date}T${time || '00:00'}:00`);
		return isNaN(d.getTime()) ? null : d.toISOString();
	}

	function fmtWhen(iso: string | null): string {
		if (!iso) return '';
		const d = new Date(iso);
		return d.toLocaleString([], { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
	}
	function fmtTime(iso: string | null): string {
		if (!iso) return '';
		return new Date(iso).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
	}
	function schedule(e: LiveEvent): string {
		if (e.status === 'live') return `Started ${fmtWhen(e.started_at)}`;
		if (e.status === 'ended') return `${fmtWhen(e.started_at)} – ${fmtTime(e.ended_at)}`;
		if (!e.scheduled_start_at) return 'No scheduled time';
		return e.scheduled_end_at
			? `${fmtWhen(e.scheduled_start_at)} – ${fmtTime(e.scheduled_end_at)}`
			: fmtWhen(e.scheduled_start_at);
	}
	function canManage(e: LiveEvent): boolean {
		const u = $currentUser;
		return !!u && (u.is_admin || e.created_by === u.id || e.host_user_id === u.id);
	}

	function openCreate() {
		form = emptyForm();
		createError = '';
		createOpen = true;
	}

	async function create() {
		createError = '';
		if (!form.title.trim()) {
			createError = 'Title is required.';
			return;
		}
		const body: LiveEventInput = {
			title: form.title.trim(),
			description: form.description.trim(),
			kind: form.kind,
			start_now: form.start_now,
			auto_start: form.auto_start,
			auto_end: form.auto_end
		};
		if (!form.start_now) {
			const start = toISO(form.start_date, form.start_time);
			const end = toISO(form.end_date || form.start_date, form.end_time);
			if (form.start_date && !start) {
				createError = 'Start time is not valid.';
				return;
			}
			if (start) body.scheduled_start_at = start;
			if (form.end_time && end) body.scheduled_end_at = end;
		} else if (form.end_time) {
			const today = new Date().toISOString().slice(0, 10);
			const end = toISO(form.end_date || today, form.end_time);
			if (!end || new Date(end) <= new Date()) {
				createError = 'The end time must be later than now.';
				return;
			}
			body.scheduled_end_at = end;
		}
		if (form.join_url.trim()) body.join_url = form.join_url.trim();
		if (form.host_user_id) body.host_user_id = form.host_user_id;
		creating = true;
		try {
			const ev = await api.post<LiveEvent>('/v1/live-events', body);
			items = [ev, ...items];
			createOpen = false;
			toast.success(ev.status === 'live' ? 'You are live.' : 'Live event scheduled.');
			await load(true);
		} catch (e: any) {
			createError = e.message || 'Could not create the live event';
		} finally {
			creating = false;
		}
	}

	function replace(ev: LiveEvent) {
		items = items.map((e) => (e.id === ev.id ? ev : e));
	}

	async function start(e: LiveEvent) {
		try {
			replace(await api.post<LiveEvent>(`/v1/live-events/${e.id}/start`));
			toast.success(`"${e.title}" is live.`);
		} catch (err: any) {
			toast.error(err.message || 'Could not start');
		}
	}

	function askEnd(e: LiveEvent) {
		confirmTitle = 'End this session?';
		confirmDescription = `"${e.title}" will go offline and its join link will be withdrawn from the live page and widget.`;
		confirmActionText = 'End session';
		confirmDestructive = false;
		pendingAction = async () => {
			try {
				replace(await api.post<LiveEvent>(`/v1/live-events/${e.id}/end`));
				toast.success('Session ended.');
			} catch (err: any) {
				toast.error(err.message || 'Could not end');
			}
		};
		confirmOpen = true;
	}

	function askCancel(e: LiveEvent) {
		confirmTitle = e.status === 'live' ? 'End and cancel this session?' : 'Cancel this live event?';
		confirmDescription = `"${e.title}" will be cancelled${e.has_calendar_event ? ' and removed from the host’s calendar' : ''}. This cannot be undone.`;
		confirmActionText = 'Cancel event';
		confirmDestructive = true;
		pendingAction = async () => {
			try {
				replace(await api.del<LiveEvent>(`/v1/live-events/${e.id}`));
				toast.success('Live event cancelled.');
			} catch (err: any) {
				toast.error(err.message || 'Could not cancel');
			}
		};
		confirmOpen = true;
	}

	function copy(text: string, what: string) {
		navigator.clipboard
			.writeText(text)
			.then(() => toast.success(`${what} copied.`))
			.catch(() => toast.error('Could not copy'));
	}

	function badgeVariant(s: LiveEvent['status']): 'default' | 'secondary' | 'outline' | 'destructive' {
		switch (s) {
			case 'live':
				return 'destructive';
			case 'scheduled':
				return 'default';
			case 'cancelled':
				return 'outline';
			default:
				return 'secondary';
		}
	}
</script>

<svelte:head><title>Live events — Calnode</title></svelte:head>

<ConfirmDialog
	bind:open={confirmOpen}
	title={confirmTitle}
	description={confirmDescription}
	confirmText={confirmActionText}
	destructive={confirmDestructive}
	onConfirm={() => pendingAction?.()}
/>

<Dialog.Root bind:open={createOpen}>
	<Dialog.Content class="max-w-lg">
		<Dialog.Header>
			<Dialog.Title>New live event</Dialog.Title>
			<Dialog.Description>
				The join link is a Google Meet (or Teams) on the host's connected calendar; the workspace's default
				participants are invited automatically. Paste a link to use your own instead.
			</Dialog.Description>
		</Dialog.Header>

		{#if createError}
			<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{createError}</p>
		{/if}

		<div class="space-y-4">
			<div class="space-y-1.5">
				<Label for="le-title">Title</Label>
				<Input id="le-title" bind:value={form.title} placeholder="Office hours" maxlength={200} />
			</div>
			<div class="space-y-1.5">
				<Label for="le-desc">Description <span class="font-normal text-muted-foreground">(optional)</span></Label>
				<Textarea id="le-desc" rows={3} bind:value={form.description} placeholder="Drop in with questions about…" />
			</div>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div class="space-y-1.5">
					<Label for="le-kind">Kind</Label>
					<Select.Root type="single" value={form.kind} onValueChange={(v) => { if (v) form.kind = v as LiveEvent['kind']; }}>
						<Select.Trigger id="le-kind" class="w-full">{KIND_LABELS[form.kind]}</Select.Trigger>
						<Select.Content>
							<Select.Item value="office_hours" label="Office hours">Office hours</Select.Item>
							<Select.Item value="event" label="Event">Event</Select.Item>
						</Select.Content>
					</Select.Root>
				</div>
				{#if $currentUser?.is_admin && members.length > 0}
					<div class="space-y-1.5">
						<Label for="le-host">Host</Label>
						<Select.Root type="single" value={form.host_user_id} onValueChange={(v) => (form.host_user_id = v ?? '')}>
							<Select.Trigger id="le-host" class="w-full">
								{members.find((m) => m.id === form.host_user_id)?.name ?? 'Me'}
							</Select.Trigger>
							<Select.Content>
								<Select.Item value="" label="Me">Me</Select.Item>
								{#each members as m (m.id)}
									<Select.Item value={m.id} label={m.name}>{m.name} · {m.email}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
					</div>
				{/if}
			</div>

			<label class="flex items-center gap-2 text-sm">
				<Checkbox bind:checked={form.start_now} />
				Start now
			</label>

			{#if !form.start_now}
				<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
					<div class="space-y-1.5">
						<Label for="le-start-time">Starts</Label>
						<div class="flex flex-wrap gap-2">
							<DatePicker bind:value={form.start_date} placeholder="Date" minToday class="w-[150px]" />
							<Input id="le-start-time" type="time" bind:value={form.start_time} class="w-[120px]" />
						</div>
					</div>
					<div class="space-y-1.5">
						<Label for="le-end-time">Ends <span class="font-normal text-muted-foreground">(optional)</span></Label>
						<div class="flex flex-wrap gap-2">
							<DatePicker bind:value={form.end_date} placeholder="Same day" minToday class="w-[150px]" />
							<Input id="le-end-time" type="time" bind:value={form.end_time} class="w-[120px]" />
						</div>
					</div>
				</div>
				<p class="text-xs text-muted-foreground">Times are in your local timezone. Leave blank to start it by hand.</p>
			{:else}
				<div class="space-y-1.5">
					<Label for="le-end-time-now">Ends <span class="font-normal text-muted-foreground">(optional, today)</span></Label>
					<Input id="le-end-time-now" type="time" bind:value={form.end_time} class="w-[120px]" />
				</div>
			{/if}

			<div class="space-y-1.5">
				<Label for="le-join">Join link <span class="font-normal text-muted-foreground">(optional override)</span></Label>
				<Input id="le-join" type="url" bind:value={form.join_url} placeholder="https://…" />
			</div>

			<div class="flex flex-wrap gap-6">
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={form.auto_start} />
					Go live automatically at the start time
				</label>
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={form.auto_end} />
					End automatically at the end time
				</label>
			</div>
		</div>

		<Dialog.Footer class="mt-2">
			<Button variant="outline" onclick={() => (createOpen = false)} disabled={creating}>Cancel</Button>
			<Button onclick={create} disabled={creating}>
				{creating ? 'Saving…' : form.start_now ? 'Go live' : 'Schedule'}
			</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<div class="mb-8 flex items-center justify-between">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">Live events</h1>
		<p class="mt-1 text-sm text-muted-foreground">
			Office hours and live sessions. The join link is published on your live page and widget while a session is on.
		</p>
	</div>
	<Button onclick={openCreate}>New live event</Button>
</div>

{#if loading}
	<p class="text-sm text-muted-foreground">Loading…</p>
{:else}
	<div class="mb-3 flex items-center justify-between">
		<p class="text-sm text-muted-foreground">
			{#if liveCount > 0}
				<span class="font-medium text-foreground">{liveCount} live now</span> ·
			{/if}
			{visible.length} shown
		</p>
		<label class="flex items-center gap-2 text-sm text-muted-foreground">
			<Switch bind:checked={showEnded} />
			Show ended and cancelled
		</label>
	</div>

	{#if visible.length === 0}
		<div class="rounded-lg border border-dashed p-10 text-center">
			<p class="text-sm text-muted-foreground">No live events yet. Schedule office hours or go live right away.</p>
		</div>
	{:else}
		<div class="overflow-x-auto rounded-lg border bg-card">
			<table class="w-full text-sm">
				<thead class="border-b bg-muted/50 text-left text-xs uppercase tracking-wide text-muted-foreground">
					<tr>
						<th class="px-4 py-3">Status</th>
						<th class="px-4 py-3">Session</th>
						<th class="px-4 py-3">Host</th>
						<th class="px-4 py-3">When</th>
						<th class="px-4 py-3">Join link</th>
						<th class="px-4 py-3 text-right">Actions</th>
					</tr>
				</thead>
				<tbody>
					{#each visible as e (e.id)}
						<tr class="border-b last:border-0">
							<td class="px-4 py-3">
								<Badge variant={badgeVariant(e.status)} class={e.status === 'live' ? 'gap-1.5' : ''}>
									{#if e.status === 'live'}
										<span class="relative flex size-2">
											<span class="absolute inline-flex size-full animate-ping rounded-full bg-current opacity-75"></span>
											<span class="relative inline-flex size-2 rounded-full bg-current"></span>
										</span>
										Live
									{:else}
										{e.status[0].toUpperCase() + e.status.slice(1)}
									{/if}
								</Badge>
							</td>
							<td class="px-4 py-3">
								<p class="font-medium">{e.title}</p>
								<p class="text-xs text-muted-foreground">{KIND_LABELS[e.kind]}{e.auto_start && e.status === 'scheduled' && e.scheduled_start_at ? ' · auto-start' : ''}</p>
							</td>
							<td class="px-4 py-3">{e.host_name || '—'}</td>
							<td class="px-4 py-3 whitespace-nowrap">{schedule(e)}</td>
							<td class="px-4 py-3">
								{#if e.join_url}
									<Tooltip.Provider>
										<div class="flex items-center gap-1">
											<Tooltip.Root>
												<Tooltip.Trigger
													class={buttonVariants({ variant: 'ghost', size: 'icon' })}
													onclick={() => copy(e.join_url, 'Join link')}
												>
													<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="8" y="8" width="14" height="14" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>
												</Tooltip.Trigger>
												<Tooltip.Content>Copy join link</Tooltip.Content>
											</Tooltip.Root>
											<span class="max-w-[180px] truncate text-xs text-muted-foreground">{e.join_url}</span>
										</div>
									</Tooltip.Provider>
								{:else if e.status === 'scheduled'}
									<span class="text-xs text-muted-foreground">Minted at start</span>
								{:else}
									<span class="text-xs text-muted-foreground">—</span>
								{/if}
							</td>
							<td class="px-4 py-3">
								{#if canManage(e) && (e.status === 'scheduled' || e.status === 'live')}
									<Tooltip.Provider>
										<div class="flex items-center justify-end gap-1">
											{#if e.status === 'scheduled'}
												<Tooltip.Root>
													<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => start(e)}>
														<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="5 3 19 12 5 21 5 3"/></svg>
													</Tooltip.Trigger>
													<Tooltip.Content>Start now</Tooltip.Content>
												</Tooltip.Root>
											{:else}
												<Tooltip.Root>
													<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => askEnd(e)}>
														<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="5" width="14" height="14" rx="2"/></svg>
													</Tooltip.Trigger>
													<Tooltip.Content>End session</Tooltip.Content>
												</Tooltip.Root>
											{/if}
											<Tooltip.Root>
												<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => askCancel(e)}>
													<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-destructive"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
												</Tooltip.Trigger>
												<Tooltip.Content>Cancel event</Tooltip.Content>
											</Tooltip.Root>
										</div>
									</Tooltip.Provider>
								{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}

	<div class="mt-8 rounded-lg border bg-card p-6">
		<div class="mb-4 flex items-start justify-between gap-4">
			<div>
				<h2 class="text-sm font-semibold">Embed</h2>
				<p class="mt-1 text-sm text-muted-foreground">
					Show "Live now — join" or "Offline, next session at …" on your own site. Both poll every 30 seconds.
				</p>
			</div>
			<a href="{origin}/live" target="_blank" rel="noopener noreferrer" class={buttonVariants({ variant: 'outline', size: 'sm' })}>
				Open live page
			</a>
		</div>
		<div class="space-y-4">
			<div class="space-y-1.5">
				<div class="flex items-center justify-between">
					<Label>Iframe</Label>
					<Button variant="ghost" size="sm" onclick={() => copy(iframeSnippet, 'Iframe snippet')}>Copy</Button>
				</div>
				<pre class="overflow-x-auto rounded-md bg-muted p-3 text-xs"><code>{iframeSnippet}</code></pre>
				<p class="text-xs text-muted-foreground">Add <code>?kind=event</code> or <code>?theme=dark</code> to the URL to filter or match a dark site.</p>
			</div>
			<div class="space-y-1.5">
				<div class="flex items-center justify-between">
					<Label>Widget</Label>
					<Button variant="ghost" size="sm" onclick={() => copy(widgetSnippet, 'Widget snippet')}>Copy</Button>
				</div>
				<pre class="overflow-x-auto rounded-md bg-muted p-3 text-xs"><code>{widgetSnippet}</code></pre>
				<p class="text-xs text-muted-foreground">
					Inline web component. Drop <code>data-kind</code> to show every kind; <code>data-base</code> defaults to this server.
				</p>
			</div>
			<p class="text-xs text-muted-foreground">
				Raw status for your own app: <code>GET {origin}/v1/live/status</code> (public JSON, CORS-open).
			</p>
		</div>
	</div>
{/if}
