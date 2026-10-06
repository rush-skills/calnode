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

	// Kinds are free-form slugs; show "office_hours" as "Office hours".
	function kindLabel(k: string) {
		const t = (k || '').replace(/[_-]+/g, ' ').trim();
		return t ? t[0].toUpperCase() + t.slice(1) : '';
	}
	function normKind(k: string) {
		return k.trim().toLowerCase().replace(/[^a-z0-9_-]+/g, '_').replace(/^[_-]+/, '').slice(0, 40);
	}
	const POLL_MS = 30_000;

	let items = $state<LiveEvent[]>([]);
	// Kinds already used, offered as suggestions in the form.
	let loading = $state(true);
	// History: ended and cancelled sessions, paged from the server and filterable by kind.
	const HISTORY_PAGE = 25;
	let history = $state<LiveEvent[]>([]);
	let historyKind = $state('');
	let historyMore = $state(false);
	let historyLoading = $state(false);
	// Embed card: which kind the snippets are for ('' = every kind).
	let embedKind = $state('');
	const knownKinds = $derived(
		Array.from(new Set(['office_hours', ...items.map((e) => e.kind), ...history.map((e) => e.kind)])).sort()
	);
	let members = $state<TeamMember[]>([]);
	let pollTimer: ReturnType<typeof setInterval> | null = null;

	// ---- Create dialog ----
	let createOpen = $state(false);
	let creating = $state(false);
	let createError = $state('');
	const emptyForm = () => ({
		title: '',
		description: '',
		kind: 'office_hours',
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

	const visible = $derived(items);
	const liveCount = $derived(items.filter((e) => e.status === 'live').length);
	const origin = $derived(typeof window !== 'undefined' ? window.location.origin : '');
	const livePageURL = $derived(`${origin}/live${embedKind ? `?kind=${encodeURIComponent(embedKind)}` : ''}`);
	const iframeSnippet = $derived(
		`<iframe src="${livePageURL}" title="${embedKind ? kindLabel(embedKind) : 'Live now'}" width="100%" height="320" style="border:0;border-radius:12px" loading="lazy"></iframe>`
	);
	const widgetSnippet = $derived(
		`<script src="${origin}/live-widget.js" async><\/script>\n<calnode-live${embedKind ? ` data-kind="${embedKind}"` : ''} data-poll="30"></calnode-live>`
	);

	async function load(quiet = false) {
		try {
			const res = await api.get<{ live_events: LiveEvent[] }>('/v1/live-events?status=active&limit=200');
			items = res.live_events ?? [];
		} catch (e: any) {
			if (!quiet) toast.error(e.message || 'Could not load live events');
		} finally {
			loading = false;
		}
	}

	async function loadHistory(reset = false) {
		historyLoading = true;
		try {
			const offset = reset ? 0 : history.length;
			const kindQ = historyKind ? `&kind=${encodeURIComponent(historyKind)}` : '';
			const res = await api.get<{ live_events: LiveEvent[] }>(
				`/v1/live-events?status=history&limit=${HISTORY_PAGE}&offset=${offset}${kindQ}`
			);
			const page = res.live_events ?? [];
			history = reset ? page : [...history, ...page];
			historyMore = page.length === HISTORY_PAGE;
		} catch (e: any) {
			toast.error(e.message || 'Could not load session history');
		} finally {
			historyLoading = false;
		}
	}
	function duration(e: LiveEvent): string {
		if (!e.started_at || !e.ended_at) return '—';
		const mins = Math.max(0, Math.round((Date.parse(e.ended_at) - Date.parse(e.started_at)) / 60000));
		return mins >= 60 ? `${Math.floor(mins / 60)}h ${mins % 60}m` : `${mins}m`;
	}

	onMount(async () => {
		await load();
		loadHistory(true);
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
	// The viewer's LOCAL calendar day as YYYY-MM-DD. toISOString() would give the UTC day,
	// which after ~19:00 in UTC-5 (or before 05:30 in Kolkata) is a different date.
	function localYMD(d = new Date()): string {
		const p = (n: number) => String(n).padStart(2, '0');
		return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
	}
	// RFC3339 → the local date and HH:MM the form inputs want (the inverse of toISO).
	function fromISO(iso: string | null): { date: string; time: string } {
		if (!iso) return { date: '', time: '' };
		const d = new Date(iso);
		if (isNaN(d.getTime())) return { date: '', time: '' };
		const p = (n: number) => String(n).padStart(2, '0');
		return { date: localYMD(d), time: `${p(d.getHours())}:${p(d.getMinutes())}` };
	}
	// A typed time with no date to attach it to would otherwise be dropped on the floor:
	// this turns it into an inline error instead. Returns the message, or '' when fine.
	function scheduleProblem(f: { start_date: string; start_time: string; end_date: string; end_time: string }): string {
		if (f.start_time && !f.start_date) return 'Pick a start date to go with the start time.';
		if (f.end_time && !f.end_date && !f.start_date) return 'Pick a date to go with the end time.';
		if (f.end_date && !f.end_time) return 'Enter an end time to go with the end date.';
		return '';
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
			kind: normKind(form.kind) || 'office_hours',
			start_now: form.start_now,
			auto_start: form.auto_start,
			auto_end: form.auto_end
		};
		if (!form.start_now) {
			createError = scheduleProblem(form);
			if (createError) return;
			const start = toISO(form.start_date, form.start_time);
			const end = toISO(form.end_date || form.start_date, form.end_time);
			if (form.start_date && !start) {
				createError = 'Start time is not valid.';
				return;
			}
			if (start) body.scheduled_start_at = start;
			if (form.end_time && end) body.scheduled_end_at = end;
		} else if (form.end_time) {
			const end = toISO(form.end_date || localYMD(), form.end_time);
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

	// ---- Edit dialog (scheduled and live rows; schedule and host are fixed once live) ----
	let editOpen = $state(false);
	let saving = $state(false);
	let editError = $state('');
	let editTarget = $state<LiveEvent | null>(null);
	let editForm = $state({
		title: '',
		description: '',
		kind: '',
		join_url: '',
		start_date: '',
		start_time: '',
		end_date: '',
		end_time: '',
		host_user_id: '',
		auto_start: true,
		auto_end: true
	});
	const editIsLive = $derived(editTarget?.status === 'live');

	function openEdit(e: LiveEvent) {
		const s = fromISO(e.scheduled_start_at), en = fromISO(e.scheduled_end_at);
		editTarget = e;
		editForm = {
			title: e.title,
			description: e.description,
			kind: e.kind,
			join_url: e.join_url,
			start_date: s.date,
			start_time: s.time,
			end_date: en.date,
			end_time: en.time,
			host_user_id: e.host_user_id,
			auto_start: e.auto_start,
			auto_end: e.auto_end
		};
		editError = '';
		editOpen = true;
	}

	async function saveEdit() {
		if (!editTarget) return;
		editError = '';
		if (!editForm.title.trim()) {
			editError = 'Title is required.';
			return;
		}
		const body: LiveEventInput = {
			title: editForm.title.trim(),
			description: editForm.description.trim(),
			kind: normKind(editForm.kind) || 'office_hours',
			join_url: editForm.join_url.trim(),
			auto_start: editForm.auto_start,
			auto_end: editForm.auto_end
		};
		if (editIsLive && !body.join_url) {
			editError = 'A live session needs a join link; end it first to remove the link.';
			return;
		}
		if (!editIsLive) {
			editError = scheduleProblem(editForm);
			if (editError) return;
			const start = toISO(editForm.start_date, editForm.start_time);
			const end = toISO(editForm.end_date || editForm.start_date, editForm.end_time);
			if (editForm.start_date && !start) {
				editError = 'Start time is not valid.';
				return;
			}
			// "" clears a stored time; the server treats an omitted field as "keep".
			body.scheduled_start_at = start ?? '';
			body.scheduled_end_at = editForm.end_time && end ? end : '';
			// "" would mean "no change" server-side, so only a real reassignment is sent.
			if (editForm.host_user_id && editForm.host_user_id !== editTarget.host_user_id) {
				body.host_user_id = editForm.host_user_id;
			}
		}
		saving = true;
		try {
			replace(await api.patch<LiveEvent>(`/v1/live-events/${editTarget.id}`, body));
			editOpen = false;
			toast.success('Live event updated.');
		} catch (e: any) {
			editError = e.message || 'Could not save the live event';
		} finally {
			saving = false;
		}
	}

	function replace(ev: LiveEvent) {
		if (ev.status === 'ended' || ev.status === 'cancelled') {
			// Finished: out of the active list, into history.
			items = items.filter((e) => e.id !== ev.id);
			loadHistory(true);
			return;
		}
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
					<Input id="le-kind" list="le-kind-options" bind:value={form.kind} placeholder="office_hours" />
					<datalist id="le-kind-options">
						{#each knownKinds as k}<option value={k}>{kindLabel(k)}</option>{/each}
					</datalist>
					{#if form.kind.trim() && normKind(form.kind) !== form.kind.trim()}
						<p class="text-xs text-muted-foreground">Will be saved as <code class="font-mono">{normKind(form.kind) || 'office_hours'}</code>.</p>
					{/if}
					<p class="text-xs text-muted-foreground">Type a new name to create a separate kind (e.g. demo). Each kind can be shown on its own page or widget.</p>
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

<Dialog.Root bind:open={editOpen}>
	<Dialog.Content class="max-w-lg">
		<Dialog.Header>
			<Dialog.Title>Edit live event</Dialog.Title>
			<Dialog.Description>
				{#if editIsLive}
					The session is live: its schedule and host are fixed until it ends. Everything else can change.
				{:else}
					Changing the schedule or host moves the calendar event; a new host gets a fresh join link.
				{/if}
			</Dialog.Description>
		</Dialog.Header>

		{#if editError}
			<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{editError}</p>
		{/if}

		<div class="space-y-4">
			<div class="space-y-1.5">
				<Label for="le-edit-title">Title</Label>
				<Input id="le-edit-title" bind:value={editForm.title} maxlength={200} />
			</div>
			<div class="space-y-1.5">
				<Label for="le-edit-desc">Description <span class="font-normal text-muted-foreground">(optional)</span></Label>
				<Textarea id="le-edit-desc" rows={3} bind:value={editForm.description} />
			</div>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div class="space-y-1.5">
					<Label for="le-edit-kind">Kind</Label>
					<Input id="le-edit-kind" list="le-edit-kind-options" bind:value={editForm.kind} placeholder="office_hours" />
					<datalist id="le-edit-kind-options">
						{#each knownKinds as k}<option value={k}>{kindLabel(k)}</option>{/each}
					</datalist>
					{#if editForm.kind.trim() && normKind(editForm.kind) !== editForm.kind.trim()}
						<p class="text-xs text-muted-foreground">Will be saved as <code class="font-mono">{normKind(editForm.kind) || 'office_hours'}</code>.</p>
					{/if}
				</div>
				{#if $currentUser?.is_admin && members.length > 0}
					<div class="space-y-1.5">
						<Label for="le-edit-host">Host</Label>
						<Select.Root type="single" value={editForm.host_user_id} disabled={editIsLive} onValueChange={(v) => (editForm.host_user_id = v ?? '')}>
							<Select.Trigger id="le-edit-host" class="w-full">
								{members.find((m) => m.id === editForm.host_user_id)?.name ?? editTarget?.host_name ?? '—'}
							</Select.Trigger>
							<Select.Content>
								{#each members as m (m.id)}
									<Select.Item value={m.id} label={m.name}>{m.name} · {m.email}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
						{#if editIsLive}<p class="text-xs text-muted-foreground">Fixed while live.</p>{/if}
					</div>
				{/if}
			</div>

			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div class="space-y-1.5">
					<Label for="le-edit-start-time">Starts</Label>
					<div class="flex flex-wrap gap-2">
						<DatePicker bind:value={editForm.start_date} placeholder="Date" minToday disabled={editIsLive} class="w-[150px]" />
						<Input id="le-edit-start-time" type="time" bind:value={editForm.start_time} disabled={editIsLive} class="w-[120px]" />
					</div>
				</div>
				<div class="space-y-1.5">
					<Label for="le-edit-end-time">Ends <span class="font-normal text-muted-foreground">(optional)</span></Label>
					<div class="flex flex-wrap gap-2">
						<DatePicker bind:value={editForm.end_date} placeholder="Same day" minToday disabled={editIsLive} class="w-[150px]" />
						<Input id="le-edit-end-time" type="time" bind:value={editForm.end_time} disabled={editIsLive} class="w-[120px]" />
					</div>
				</div>
			</div>
			<p class="text-xs text-muted-foreground">
				{editIsLive ? 'The schedule is fixed while the session is live.' : 'Times are in your local timezone. Clear both to start it by hand.'}
			</p>

			<div class="space-y-1.5">
				<Label for="le-edit-join">Join link {#if !editIsLive}<span class="font-normal text-muted-foreground">(optional override)</span>{/if}</Label>
				<Input id="le-edit-join" type="url" bind:value={editForm.join_url} placeholder="https://…" />
				{#if !editIsLive && !editForm.join_url.trim()}
					<p class="text-xs text-muted-foreground">Left blank, a Meet/Teams link is minted from the host's calendar when the session starts.</p>
				{/if}
			</div>

			<div class="flex flex-wrap gap-6">
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={editForm.auto_start} />
					Go live automatically at the start time
				</label>
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={editForm.auto_end} />
					End automatically at the end time
				</label>
			</div>
		</div>

		<Dialog.Footer class="mt-2">
			<Button variant="outline" onclick={() => (editOpen = false)} disabled={saving}>Cancel</Button>
			<Button onclick={saveEdit} disabled={saving}>{saving ? 'Saving…' : 'Save changes'}</Button>
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
	</div>

	{#if visible.length === 0}
		<div class="rounded-lg border border-dashed p-10 text-center">
			<p class="text-sm text-muted-foreground">Nothing scheduled or live. Schedule office hours or go live right away.</p>
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
								<p class="text-xs text-muted-foreground">{kindLabel(e.kind)}{e.auto_start && e.status === 'scheduled' && e.scheduled_start_at ? ' · auto-start' : ''}</p>
							</td>
							<td class="px-4 py-3">{e.host_name || '—'}</td>
							<td class="px-4 py-3">{schedule(e)}</td>
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
								{:else if e.status === 'scheduled' && e.has_calendar_event}
									<span class="text-xs text-muted-foreground">Minted at start</span>
								{:else if e.status === 'scheduled'}
									<!-- No calendar event means nothing will mint a link: say so instead of promising one. -->
									<span class="text-xs text-muted-foreground">No link yet — edit to add one</span>
								{:else}
									<span class="text-xs text-muted-foreground">—</span>
								{/if}
							</td>
							<td class="px-4 py-3">
								{#if canManage(e) && (e.status === 'scheduled' || e.status === 'live')}
									<Tooltip.Provider>
										<div class="flex items-center justify-end gap-1">
											<Tooltip.Root>
												<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => openEdit(e)}>
													<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>
												</Tooltip.Trigger>
												<Tooltip.Content>Edit</Tooltip.Content>
											</Tooltip.Root>
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

	<div class="mt-8 rounded-lg border bg-card">
		<div class="flex flex-wrap items-center justify-between gap-3 border-b p-4">
			<div>
				<h2 class="text-sm font-semibold">Session history</h2>
				<p class="mt-0.5 text-xs text-muted-foreground">Ended and cancelled sessions, newest first.</p>
			</div>
			<Select.Root type="single" value={historyKind} onValueChange={(v) => { historyKind = v ?? ''; loadHistory(true); }}>
				<Select.Trigger class="w-48" aria-label="Filter history by kind">{historyKind ? kindLabel(historyKind) : 'All kinds'}</Select.Trigger>
				<Select.Content>
					<Select.Item value="" label="All kinds">All kinds</Select.Item>
					{#each knownKinds as k}<Select.Item value={k} label={kindLabel(k)}>{kindLabel(k)}</Select.Item>{/each}
				</Select.Content>
			</Select.Root>
		</div>
		{#if history.length === 0}
			<p class="p-6 text-center text-sm text-muted-foreground">{historyLoading ? 'Loading…' : 'No past sessions yet.'}</p>
		{:else}
			<div class="overflow-x-auto">
				<table class="w-full text-sm">
					<thead class="border-b bg-muted/50 text-left text-xs uppercase tracking-wide text-muted-foreground">
						<tr>
							<th class="px-4 py-3">Session</th>
							<th class="px-4 py-3">Host</th>
							<th class="px-4 py-3">Started</th>
							<th class="px-4 py-3">Duration</th>
							<th class="px-4 py-3">Outcome</th>
						</tr>
					</thead>
					<tbody>
						{#each history as e (e.id)}
							<tr class="border-b last:border-0">
								<td class="px-4 py-3">
									<p class="font-medium">{e.title}</p>
									<p class="text-xs text-muted-foreground">{kindLabel(e.kind)}</p>
								</td>
								<td class="px-4 py-3">{e.host_name || '—'}</td>
								<td class="px-4 py-3">{e.started_at ? fmtWhen(e.started_at) : e.scheduled_start_at ? `${fmtWhen(e.scheduled_start_at)} (scheduled)` : '—'}</td>
								<td class="px-4 py-3">{duration(e)}</td>
								<td class="px-4 py-3">
									<Badge variant={badgeVariant(e.status)}>{e.status === 'ended' ? 'Ended' : 'Cancelled'}</Badge>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if historyMore}
				<div class="border-t p-3 text-center">
					<Button variant="ghost" size="sm" disabled={historyLoading} onclick={() => loadHistory(false)}>
						{historyLoading ? 'Loading…' : 'Load more'}
					</Button>
				</div>
			{/if}
		{/if}
	</div>

	<div class="mt-8 rounded-lg border bg-card p-6">
		<div class="mb-4 flex items-start justify-between gap-4">
			<div>
				<h2 class="text-sm font-semibold">Embed</h2>
				<p class="mt-1 text-sm text-muted-foreground">
					Show "Live now — join" or "Offline, next session at …" on your own site. Both poll every 30 seconds.
				</p>
			</div>
			<a href={livePageURL} target="_blank" rel="noopener noreferrer" class={buttonVariants({ variant: 'outline', size: 'sm' })}>
				Open live page
			</a>
		</div>
		<div class="space-y-4">
			<div class="space-y-1.5">
				<Label for="embed-kind">Show sessions of</Label>
				<Select.Root type="single" value={embedKind} onValueChange={(v) => (embedKind = v ?? '')}>
					<Select.Trigger id="embed-kind" class="w-full sm:w-64">{embedKind ? kindLabel(embedKind) : 'Every kind'}</Select.Trigger>
					<Select.Content>
						<Select.Item value="" label="Every kind">Every kind</Select.Item>
						{#each knownKinds as k}<Select.Item value={k} label={kindLabel(k)}>{kindLabel(k)}</Select.Item>{/each}
					</Select.Content>
				</Select.Root>
				<p class="text-xs text-muted-foreground">Pick a kind to get snippets that show only that kind, so each can be placed somewhere different.</p>
			</div>
			<div class="space-y-1.5">
				<div class="flex items-center justify-between">
					<Label>Iframe</Label>
					<Button variant="ghost" size="sm" onclick={() => copy(iframeSnippet, 'Iframe snippet')}>Copy</Button>
				</div>
				<pre class="overflow-x-auto rounded-md bg-muted p-3 text-xs"><code>{iframeSnippet}</code></pre>
				<p class="text-xs text-muted-foreground">Add <code>&amp;theme=dark</code> (or <code>?theme=dark</code>) to match a dark site.</p>
			</div>
			<div class="space-y-1.5">
				<div class="flex items-center justify-between">
					<Label>Widget</Label>
					<Button variant="ghost" size="sm" onclick={() => copy(widgetSnippet, 'Widget snippet')}>Copy</Button>
				</div>
				<pre class="overflow-x-auto rounded-md bg-muted p-3 text-xs"><code>{widgetSnippet}</code></pre>
				<p class="text-xs text-muted-foreground">
					Inline web component. <code>data-kind</code> limits it to one kind; <code>data-base</code> defaults to this server.
				</p>
			</div>
			<p class="text-xs text-muted-foreground">
				Raw status for your own app: <code>GET {origin}/v1/live/status</code> (public JSON, CORS-open).
			</p>
		</div>
	</div>
{/if}
