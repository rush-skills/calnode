<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import {
		api,
		type LiveEvent,
		type LiveEventInput,
		type LiveSeries,
		type LiveSeriesInput,
		type TeamMember
	} from '$lib/api';
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
	import { Combobox } from '$lib/components/ui/combobox';
	import { prefs, timezoneOptions, canonicalTz, weekDays } from '$lib/prefs';
	import { t, tr, localeTag } from '$lib/i18n';
	import { toast } from 'svelte-sonner';

	// Kinds are free-form slugs; show "office_hours" as "Office hours".
	function kindLabel(k: string) {
		const s = (k || '').replace(/[_-]+/g, ' ').trim();
		return s ? s[0].toUpperCase() + s.slice(1) : '';
	}
	function normKind(k: string) {
		return k.trim().toLowerCase().replace(/[^a-z0-9_-]+/g, '_').replace(/^[_-]+/, '').slice(0, 40);
	}
	const POLL_MS = 30_000;

	let items = $state<LiveEvent[]>([]);
	let loading = $state(true);
	// History: ended and cancelled sessions, paged from the server and filterable by kind.
	const HISTORY_PAGE = 25;
	let history = $state<LiveEvent[]>([]);
	let historyKind = $state('');
	let historyMore = $state(false);
	let historyLoading = $state(false);
	// Embed card: which kind the snippets are for ('' = every kind).
	let embedKind = $state('');
	let series = $state<LiveSeries[]>([]);
	const knownKinds = $derived(
		Array.from(
			new Set(['office_hours', ...items.map((e) => e.kind), ...history.map((e) => e.kind), ...series.map((s) => s.kind)])
		).sort()
	);
	let members = $state<TeamMember[]>([]);
	let pollTimer: ReturnType<typeof setInterval> | null = null;
	const dayNames = $derived(weekDays($t));
	// Monday-first order for the weekday toggles.
	const WEEK_ORDER = [1, 2, 3, 4, 5, 6, 0];

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
		auto_end: true,
		invite_default_participants: true
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
		`<iframe src="${livePageURL}" title="${embedKind ? kindLabel(embedKind) : $t('liveEvents.embed.iframeTitle')}" width="100%" height="320" style="border:0;border-radius:12px" loading="lazy"></iframe>`
	);
	const widgetSnippet = $derived(
		`<script src="${origin}/live-widget.js" async><\/script>\n<calnode-live${embedKind ? ` data-kind="${embedKind}"` : ''} data-poll="30"></calnode-live>`
	);

	async function load(quiet = false) {
		try {
			const res = await api.get<{ live_events: LiveEvent[] }>('/v1/live-events?status=active&limit=200');
			items = res.live_events ?? [];
		} catch (e: any) {
			if (!quiet) toast.error(e.message || tr('liveEvents.error.load'));
		} finally {
			loading = false;
		}
	}

	async function loadSeries() {
		try {
			series = (await api.get<{ series: LiveSeries[] }>('/v1/live-series?status=active')).series ?? [];
		} catch (e: any) {
			toast.error(e.message || tr('liveEvents.series.error.load'));
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
			toast.error(e.message || tr('liveEvents.error.history'));
		} finally {
			historyLoading = false;
		}
	}
	function duration(e: LiveEvent): string {
		if (!e.started_at || !e.ended_at) return tr('common.emDash');
		const mins = Math.max(0, Math.round((Date.parse(e.ended_at) - Date.parse(e.started_at)) / 60000));
		return mins >= 60
			? tr('liveEvents.duration.hoursMinutes', { h: Math.floor(mins / 60), m: mins % 60 })
			: tr('liveEvents.duration.minutes', { m: mins });
	}

	onMount(async () => {
		await load();
		loadSeries();
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
		if (f.start_time && !f.start_date) return tr('liveEvents.validation.startDateForTime');
		if (f.end_time && !f.end_date && !f.start_date) return tr('liveEvents.validation.dateForEndTime');
		if (f.end_date && !f.end_time) return tr('liveEvents.validation.endTimeForDate');
		return '';
	}

	function fmtWhen(iso: string | null): string {
		if (!iso) return '';
		return new Date(iso).toLocaleString(localeTag(), { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
	}
	function fmtTime(iso: string | null): string {
		if (!iso) return '';
		return new Date(iso).toLocaleTimeString(localeTag(), { hour: 'numeric', minute: '2-digit' });
	}
	function fmtDay(ymd: string): string {
		const d = new Date(`${ymd}T12:00:00`);
		return isNaN(d.getTime()) ? ymd : d.toLocaleDateString(localeTag(), { day: 'numeric', month: 'short', year: 'numeric' });
	}
	function schedule(e: LiveEvent): string {
		if (e.status === 'live') return tr('liveEvents.when.started', { when: fmtWhen(e.started_at) });
		if (e.status === 'ended') return `${fmtWhen(e.started_at)} – ${fmtTime(e.ended_at)}`;
		if (!e.scheduled_start_at) return tr('liveEvents.when.unscheduled');
		return e.scheduled_end_at
			? `${fmtWhen(e.scheduled_start_at)} – ${fmtTime(e.scheduled_end_at)}`
			: fmtWhen(e.scheduled_start_at);
	}
	function statusLabel(s: LiveEvent['status']): string {
		return tr(`liveEvents.status.${s}`);
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
			createError = tr('liveEvents.validation.title');
			return;
		}
		const body: LiveEventInput = {
			title: form.title.trim(),
			description: form.description.trim(),
			kind: normKind(form.kind) || 'office_hours',
			start_now: form.start_now,
			auto_start: form.auto_start,
			auto_end: form.auto_end,
			invite_default_participants: form.invite_default_participants
		};
		if (!form.start_now) {
			createError = scheduleProblem(form);
			if (createError) return;
			const start = toISO(form.start_date, form.start_time);
			const end = toISO(form.end_date || form.start_date, form.end_time);
			if (form.start_date && !start) {
				createError = tr('liveEvents.validation.startInvalid');
				return;
			}
			if (start) body.scheduled_start_at = start;
			if (form.end_time && end) body.scheduled_end_at = end;
		} else if (form.end_time) {
			const end = toISO(form.end_date || localYMD(), form.end_time);
			if (!end || new Date(end) <= new Date()) {
				createError = tr('liveEvents.validation.endAfterNow');
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
			toast.success(ev.status === 'live' ? tr('liveEvents.toast.live') : tr('liveEvents.toast.scheduled'));
			await load(true);
		} catch (e: any) {
			createError = e.message || tr('liveEvents.error.create');
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
		auto_end: true,
		invite_default_participants: true
	});
	const editIsLive = $derived(editTarget?.status === 'live');

	function openEdit(e: LiveEvent) {
		const s = fromISO(e.scheduled_start_at),
			en = fromISO(e.scheduled_end_at);
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
			auto_end: e.auto_end,
			invite_default_participants: e.invite_default_participants
		};
		editError = '';
		editOpen = true;
	}

	async function saveEdit() {
		if (!editTarget) return;
		editError = '';
		if (!editForm.title.trim()) {
			editError = tr('liveEvents.validation.title');
			return;
		}
		const body: LiveEventInput = {
			title: editForm.title.trim(),
			description: editForm.description.trim(),
			kind: normKind(editForm.kind) || 'office_hours',
			join_url: editForm.join_url.trim(),
			auto_start: editForm.auto_start,
			auto_end: editForm.auto_end,
			invite_default_participants: editForm.invite_default_participants
		};
		if (editIsLive && !body.join_url) {
			editError = tr('liveEvents.validation.liveNeedsLink');
			return;
		}
		if (!editIsLive) {
			editError = scheduleProblem(editForm);
			if (editError) return;
			const start = toISO(editForm.start_date, editForm.start_time);
			const end = toISO(editForm.end_date || editForm.start_date, editForm.end_time);
			if (editForm.start_date && !start) {
				editError = tr('liveEvents.validation.startInvalid');
				return;
			}
			// "" clears a stored time; the server treats an omitted field as "keep". Only a
			// changed time is sent, so editing the title of a series occurrence does not
			// make its time an override.
			const newStart = start ?? '';
			const newEnd = editForm.end_time && end ? end : '';
			if (newStart !== (editTarget.scheduled_start_at ?? '') || newEnd !== (editTarget.scheduled_end_at ?? '')) {
				body.scheduled_start_at = newStart;
				body.scheduled_end_at = newEnd;
			}
			// "" would mean "no change" server-side, so only a real reassignment is sent.
			if (editForm.host_user_id && editForm.host_user_id !== editTarget.host_user_id) {
				body.host_user_id = editForm.host_user_id;
			}
		}
		saving = true;
		try {
			replace(await api.patch<LiveEvent>(`/v1/live-events/${editTarget.id}`, body));
			editOpen = false;
			toast.success(tr('liveEvents.toast.updated'));
		} catch (e: any) {
			editError = e.message || tr('liveEvents.error.save');
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
			toast.success(tr('liveEvents.toast.started', { title: e.title }));
		} catch (err: any) {
			toast.error(err.message || tr('liveEvents.error.start'));
		}
	}

	function askEnd(e: LiveEvent) {
		confirmTitle = tr('liveEvents.confirm.end.title');
		confirmDescription = tr('liveEvents.confirm.end.description', { title: e.title });
		confirmActionText = tr('liveEvents.confirm.end.action');
		confirmDestructive = false;
		pendingAction = async () => {
			try {
				replace(await api.post<LiveEvent>(`/v1/live-events/${e.id}/end`));
				toast.success(tr('liveEvents.toast.ended'));
			} catch (err: any) {
				toast.error(err.message || tr('liveEvents.error.end'));
			}
		};
		confirmOpen = true;
	}

	function askCancel(e: LiveEvent) {
		confirmTitle = e.status === 'live' ? tr('liveEvents.confirm.cancel.titleLive') : tr('liveEvents.confirm.cancel.title');
		confirmDescription = e.has_calendar_event
			? tr('liveEvents.confirm.cancel.descriptionCalendar', { title: e.title })
			: tr('liveEvents.confirm.cancel.description', { title: e.title });
		if (e.series_id) confirmDescription += ' ' + tr('liveEvents.confirm.cancel.seriesNote');
		confirmActionText = tr('liveEvents.confirm.cancel.action');
		confirmDestructive = true;
		pendingAction = async () => {
			try {
				replace(await api.del<LiveEvent>(`/v1/live-events/${e.id}`));
				toast.success(tr('liveEvents.toast.cancelled'));
			} catch (err: any) {
				toast.error(err.message || tr('liveEvents.error.cancel'));
			}
		};
		confirmOpen = true;
	}

	function copy(text: string, what: string) {
		navigator.clipboard
			.writeText(text)
			.then(() => toast.success(tr('liveEvents.toast.copied', { what })))
			.catch(() => toast.error(tr('common.copyClipboardError')));
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

	// ---- Recurring series ----
	let seriesOpen = $state(false);
	let seriesSaving = $state(false);
	let seriesError = $state('');
	let seriesTarget = $state<LiveSeries | null>(null);
	const browserTz = typeof Intl !== 'undefined' ? canonicalTz(Intl.DateTimeFormat().resolvedOptions().timeZone) : 'UTC';
	const emptySeries = () => ({
		title: '',
		description: '',
		kind: 'office_hours',
		host_user_ids: [] as string[],
		timezone: canonicalTz($prefs.timezone || browserTz),
		weekdays: [] as number[],
		interval_weeks: 1,
		start_time: '17:00',
		duration_minutes: 60,
		start_date: localYMD(),
		end_date: '',
		skip_dates: [] as string[],
		link_mode: 'fresh' as 'fresh' | 'fixed',
		join_url: '',
		auto_start: true,
		auto_end: true,
		invite_default_participants: true
	});
	let sForm = $state(emptySeries());
	let skipFrom = $state('');
	let skipTo = $state('');

	function openSeries(s: LiveSeries | null) {
		seriesTarget = s;
		seriesError = '';
		skipFrom = skipTo = '';
		sForm = s
			? {
					title: s.title,
					description: s.description,
					kind: s.kind,
					host_user_ids: [...s.host_user_ids],
					timezone: s.timezone,
					weekdays: [...s.weekdays],
					interval_weeks: s.interval_weeks,
					start_time: s.start_time,
					duration_minutes: s.duration_minutes,
					start_date: s.start_date,
					end_date: s.end_date ?? '',
					skip_dates: [...s.skip_dates],
					link_mode: s.join_url ? 'fixed' : 'fresh',
					join_url: s.join_url,
					auto_start: s.auto_start,
					auto_end: s.auto_end,
					invite_default_participants: s.invite_default_participants
				}
			: emptySeries();
		seriesOpen = true;
	}
	function toggleDay(d: number) {
		sForm.weekdays = sForm.weekdays.includes(d) ? sForm.weekdays.filter((x) => x !== d) : [...sForm.weekdays, d];
	}
	function toggleHost(id: string) {
		sForm.host_user_ids = sForm.host_user_ids.includes(id)
			? sForm.host_user_ids.filter((x) => x !== id)
			: [...sForm.host_user_ids, id];
	}
	function addSkip() {
		if (!skipFrom) return;
		const to = skipTo && skipTo !== skipFrom ? skipTo : '';
		if (to && to < skipFrom) {
			seriesError = tr('liveEvents.series.validation.skipRange');
			return;
		}
		const entry = to ? `${skipFrom}/${to}` : skipFrom;
		if (!sForm.skip_dates.includes(entry)) sForm.skip_dates = [...sForm.skip_dates, entry].sort();
		skipFrom = skipTo = '';
	}
	function skipLabel(s: string): string {
		const [a, b] = s.split('/');
		return b ? tr('liveEvents.series.skipRangeLabel', { from: fmtDay(a), to: fmtDay(b) }) : fmtDay(a);
	}
	function seriesSummary(s: LiveSeries): string {
		const days = WEEK_ORDER.filter((d) => s.weekdays.includes(d))
			.map((d) => dayNames[d])
			.join(', ');
		const every =
			s.interval_weeks === 1 ? tr('liveEvents.series.everyWeek') : tr('liveEvents.series.everyNWeeks', { count: s.interval_weeks });
		return tr('liveEvents.series.summary', { days, time: s.start_time, zone: s.timezone, every });
	}
	function seriesHosts(s: LiveSeries): string {
		const names = s.hosts.map((h) => h.name || h.id);
		return names.length > 1 ? tr('liveEvents.series.rotates', { names: names.join(', ') }) : (names[0] ?? tr('common.emDash'));
	}
	function canManageSeries(s: LiveSeries): boolean {
		const u = $currentUser;
		return !!u && (u.is_admin || s.created_by === u.id || s.host_user_ids.includes(u.id));
	}

	async function saveSeries() {
		seriesError = '';
		if (!sForm.title.trim()) return void (seriesError = tr('liveEvents.validation.title'));
		if (sForm.weekdays.length === 0) return void (seriesError = tr('liveEvents.series.validation.weekdays'));
		if (!/^\d{2}:\d{2}$/.test(sForm.start_time)) return void (seriesError = tr('liveEvents.series.validation.time'));
		if (!(sForm.duration_minutes >= 1)) return void (seriesError = tr('liveEvents.series.validation.duration'));
		if (!sForm.start_date) return void (seriesError = tr('liveEvents.series.validation.startDate'));
		if (sForm.end_date && sForm.end_date < sForm.start_date) return void (seriesError = tr('liveEvents.series.validation.endDate'));
		if (sForm.link_mode === 'fixed' && !sForm.join_url.trim()) return void (seriesError = tr('liveEvents.series.validation.link'));
		const body: LiveSeriesInput = {
			title: sForm.title.trim(),
			description: sForm.description.trim(),
			kind: normKind(sForm.kind) || 'office_hours',
			timezone: sForm.timezone,
			weekdays: sForm.weekdays,
			interval_weeks: Math.max(1, Math.floor(sForm.interval_weeks || 1)),
			start_time: sForm.start_time,
			duration_minutes: Math.floor(sForm.duration_minutes),
			start_date: sForm.start_date,
			end_date: sForm.end_date,
			skip_dates: sForm.skip_dates,
			join_url: sForm.link_mode === 'fixed' ? sForm.join_url.trim() : '',
			auto_start: sForm.auto_start,
			auto_end: sForm.auto_end,
			invite_default_participants: sForm.invite_default_participants
		};
		if ($currentUser?.is_admin && sForm.host_user_ids.length > 0) body.host_user_ids = sForm.host_user_ids;
		seriesSaving = true;
		try {
			if (seriesTarget) await api.patch(`/v1/live-series/${seriesTarget.id}`, body);
			else await api.post('/v1/live-series', body);
			seriesOpen = false;
			toast.success(seriesTarget ? tr('liveEvents.series.toast.updated') : tr('liveEvents.series.toast.created'));
			await Promise.all([loadSeries(), load(true)]);
		} catch (e: any) {
			seriesError = e.message || tr('liveEvents.series.error.save');
		} finally {
			seriesSaving = false;
		}
	}

	function askEndSeries(s: LiveSeries) {
		confirmTitle = tr('liveEvents.series.confirmEnd.title');
		confirmDescription = tr('liveEvents.series.confirmEnd.description', { title: s.title });
		confirmActionText = tr('liveEvents.series.confirmEnd.action');
		confirmDestructive = true;
		pendingAction = async () => {
			try {
				await api.del(`/v1/live-series/${s.id}`);
				toast.success(tr('liveEvents.series.toast.ended'));
				await Promise.all([loadSeries(), load(true)]);
			} catch (err: any) {
				toast.error(err.message || tr('liveEvents.series.error.end'));
			}
		};
		confirmOpen = true;
	}
</script>

<svelte:head><title>{$t('liveEvents.headTitle')}</title></svelte:head>

<ConfirmDialog
	bind:open={confirmOpen}
	title={confirmTitle}
	description={confirmDescription}
	confirmText={confirmActionText}
	destructive={confirmDestructive}
	onConfirm={() => pendingAction?.()}
/>

{#snippet kindField(id: string, value: string, set: (v: string) => void, showHint: boolean)}
	<div class="space-y-1.5">
		<Label for={id}>{$t('liveEvents.form.kind')}</Label>
		<Input {id} list="{id}-options" {value} oninput={(e) => set((e.currentTarget as HTMLInputElement).value)} placeholder="office_hours" />
		<datalist id="{id}-options">
			{#each knownKinds as k}<option value={k}>{kindLabel(k)}</option>{/each}
		</datalist>
		{#if value.trim() && normKind(value) !== value.trim()}
			<p class="text-xs text-muted-foreground">
				{$t('liveEvents.form.kindSavedAs')} <code class="font-mono">{normKind(value) || 'office_hours'}</code>
			</p>
		{/if}
		{#if showHint}<p class="text-xs text-muted-foreground">{$t('liveEvents.form.kindHint')}</p>{/if}
	</div>
{/snippet}

{#snippet inviteSwitch(checked: boolean, set: (v: boolean) => void)}
	<label class="flex items-start gap-2 text-sm">
		<Switch {checked} onCheckedChange={set} />
		<span>
			{$t('liveEvents.form.invite')}
			<span class="block text-xs text-muted-foreground">{$t('liveEvents.form.inviteHint')}</span>
		</span>
	</label>
{/snippet}

<Dialog.Root bind:open={createOpen}>
	<Dialog.Content class="max-w-lg">
		<Dialog.Header>
			<Dialog.Title>{$t('liveEvents.create.title')}</Dialog.Title>
			<Dialog.Description>{$t('liveEvents.create.description')}</Dialog.Description>
		</Dialog.Header>

		{#if createError}
			<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{createError}</p>
		{/if}

		<div class="space-y-4">
			<div class="space-y-1.5">
				<Label for="le-title">{$t('liveEvents.form.title')}</Label>
				<Input id="le-title" bind:value={form.title} placeholder={$t('liveEvents.form.titlePlaceholder')} maxlength={200} />
			</div>
			<div class="space-y-1.5">
				<Label for="le-desc">{$t('liveEvents.form.description')} <span class="font-normal text-muted-foreground">{$t('common.optional')}</span></Label>
				<Textarea id="le-desc" rows={3} bind:value={form.description} placeholder={$t('liveEvents.form.descriptionPlaceholder')} />
			</div>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				{@render kindField('le-kind', form.kind, (v) => (form.kind = v), true)}
				{#if $currentUser?.is_admin && members.length > 0}
					<div class="space-y-1.5">
						<Label for="le-host">{$t('liveEvents.form.host')}</Label>
						<Select.Root type="single" value={form.host_user_id} onValueChange={(v) => (form.host_user_id = v ?? '')}>
							<Select.Trigger id="le-host" class="w-full">
								{members.find((m) => m.id === form.host_user_id)?.name ?? $t('liveEvents.form.me')}
							</Select.Trigger>
							<Select.Content>
								<Select.Item value="" label={$t('liveEvents.form.me')}>{$t('liveEvents.form.me')}</Select.Item>
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
				{$t('liveEvents.form.startNow')}
			</label>

			{#if !form.start_now}
				<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
					<div class="space-y-1.5">
						<Label for="le-start-time">{$t('liveEvents.form.starts')}</Label>
						<div class="flex flex-wrap gap-2">
							<DatePicker bind:value={form.start_date} placeholder={$t('liveEvents.form.date')} minToday class="w-[150px]" />
							<Input id="le-start-time" type="time" bind:value={form.start_time} class="w-[120px]" />
						</div>
					</div>
					<div class="space-y-1.5">
						<Label for="le-end-time">{$t('liveEvents.form.ends')} <span class="font-normal text-muted-foreground">{$t('common.optional')}</span></Label>
						<div class="flex flex-wrap gap-2">
							<DatePicker bind:value={form.end_date} placeholder={$t('liveEvents.form.sameDay')} minToday class="w-[150px]" />
							<Input id="le-end-time" type="time" bind:value={form.end_time} class="w-[120px]" />
						</div>
					</div>
				</div>
				<p class="text-xs text-muted-foreground">{$t('liveEvents.form.localTimeHint')}</p>
			{:else}
				<div class="space-y-1.5">
					<Label for="le-end-time-now">{$t('liveEvents.form.ends')} <span class="font-normal text-muted-foreground">{$t('liveEvents.form.optionalToday')}</span></Label>
					<Input id="le-end-time-now" type="time" bind:value={form.end_time} class="w-[120px]" />
				</div>
			{/if}

			<div class="space-y-1.5">
				<Label for="le-join">{$t('liveEvents.form.joinLink')} <span class="font-normal text-muted-foreground">{$t('liveEvents.form.optionalOverride')}</span></Label>
				<Input id="le-join" type="url" bind:value={form.join_url} placeholder="https://…" />
			</div>

			<div class="flex flex-wrap gap-6">
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={form.auto_start} />
					{$t('liveEvents.form.autoStart')}
				</label>
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={form.auto_end} />
					{$t('liveEvents.form.autoEnd')}
				</label>
			</div>
			{@render inviteSwitch(form.invite_default_participants, (v) => (form.invite_default_participants = v))}
		</div>

		<Dialog.Footer class="mt-2">
			<Button variant="outline" onclick={() => (createOpen = false)} disabled={creating}>{$t('common.cancel')}</Button>
			<Button onclick={create} disabled={creating}>
				{creating ? $t('common.saving') : form.start_now ? $t('liveEvents.create.goLive') : $t('liveEvents.create.schedule')}
			</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={editOpen}>
	<Dialog.Content class="max-w-lg">
		<Dialog.Header>
			<Dialog.Title>{$t('liveEvents.edit.title')}</Dialog.Title>
			<Dialog.Description>
				{editIsLive ? $t('liveEvents.edit.descriptionLive') : $t('liveEvents.edit.description')}
				{#if editTarget?.series_id}{$t('liveEvents.edit.occurrenceNote')}{/if}
			</Dialog.Description>
		</Dialog.Header>

		{#if editError}
			<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{editError}</p>
		{/if}

		<div class="space-y-4">
			<div class="space-y-1.5">
				<Label for="le-edit-title">{$t('liveEvents.form.title')}</Label>
				<Input id="le-edit-title" bind:value={editForm.title} maxlength={200} />
			</div>
			<div class="space-y-1.5">
				<Label for="le-edit-desc">{$t('liveEvents.form.description')} <span class="font-normal text-muted-foreground">{$t('common.optional')}</span></Label>
				<Textarea id="le-edit-desc" rows={3} bind:value={editForm.description} />
			</div>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				{@render kindField('le-edit-kind', editForm.kind, (v) => (editForm.kind = v), false)}
				{#if $currentUser?.is_admin && members.length > 0}
					<div class="space-y-1.5">
						<Label for="le-edit-host">{$t('liveEvents.form.host')}</Label>
						<Select.Root type="single" value={editForm.host_user_id} disabled={editIsLive} onValueChange={(v) => (editForm.host_user_id = v ?? '')}>
							<Select.Trigger id="le-edit-host" class="w-full">
								{members.find((m) => m.id === editForm.host_user_id)?.name ?? editTarget?.host_name ?? $t('common.emDash')}
							</Select.Trigger>
							<Select.Content>
								{#each members as m (m.id)}
									<Select.Item value={m.id} label={m.name}>{m.name} · {m.email}</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
						{#if editIsLive}<p class="text-xs text-muted-foreground">{$t('liveEvents.edit.fixedWhileLive')}</p>{/if}
					</div>
				{/if}
			</div>

			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div class="space-y-1.5">
					<Label for="le-edit-start-time">{$t('liveEvents.form.starts')}</Label>
					<div class="flex flex-wrap gap-2">
						<DatePicker bind:value={editForm.start_date} placeholder={$t('liveEvents.form.date')} minToday disabled={editIsLive} class="w-[150px]" />
						<Input id="le-edit-start-time" type="time" bind:value={editForm.start_time} disabled={editIsLive} class="w-[120px]" />
					</div>
				</div>
				<div class="space-y-1.5">
					<Label for="le-edit-end-time">{$t('liveEvents.form.ends')} <span class="font-normal text-muted-foreground">{$t('common.optional')}</span></Label>
					<div class="flex flex-wrap gap-2">
						<DatePicker bind:value={editForm.end_date} placeholder={$t('liveEvents.form.sameDay')} minToday disabled={editIsLive} class="w-[150px]" />
						<Input id="le-edit-end-time" type="time" bind:value={editForm.end_time} disabled={editIsLive} class="w-[120px]" />
					</div>
				</div>
			</div>
			<p class="text-xs text-muted-foreground">
				{editIsLive ? $t('liveEvents.edit.scheduleFixed') : $t('liveEvents.edit.localTimeHint')}
			</p>

			<div class="space-y-1.5">
				<Label for="le-edit-join">
					{$t('liveEvents.form.joinLink')}
					{#if !editIsLive}<span class="font-normal text-muted-foreground">{$t('liveEvents.form.optionalOverride')}</span>{/if}
				</Label>
				<Input id="le-edit-join" type="url" bind:value={editForm.join_url} placeholder="https://…" />
				{#if !editIsLive && !editForm.join_url.trim()}
					<p class="text-xs text-muted-foreground">{$t('liveEvents.edit.mintHint')}</p>
				{/if}
			</div>

			<div class="flex flex-wrap gap-6">
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={editForm.auto_start} />
					{$t('liveEvents.form.autoStart')}
				</label>
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={editForm.auto_end} />
					{$t('liveEvents.form.autoEnd')}
				</label>
			</div>
			{@render inviteSwitch(editForm.invite_default_participants, (v) => (editForm.invite_default_participants = v))}
		</div>

		<Dialog.Footer class="mt-2">
			<Button variant="outline" onclick={() => (editOpen = false)} disabled={saving}>{$t('common.cancel')}</Button>
			<Button onclick={saveEdit} disabled={saving}>{saving ? $t('common.saving') : $t('liveEvents.edit.save')}</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={seriesOpen}>
	<Dialog.Content class="max-h-[90vh] max-w-2xl overflow-y-auto">
		<Dialog.Header>
			<Dialog.Title>{seriesTarget ? $t('liveEvents.series.editTitle') : $t('liveEvents.series.createTitle')}</Dialog.Title>
			<Dialog.Description>
				{seriesTarget ? $t('liveEvents.series.editDescription') : $t('liveEvents.series.createDescription')}
			</Dialog.Description>
		</Dialog.Header>

		{#if seriesError}
			<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{seriesError}</p>
		{/if}

		<div class="space-y-4">
			<div class="space-y-1.5">
				<Label for="ls-title">{$t('liveEvents.form.title')}</Label>
				<Input id="ls-title" bind:value={sForm.title} placeholder={$t('liveEvents.form.titlePlaceholder')} maxlength={200} />
			</div>
			<div class="space-y-1.5">
				<Label for="ls-desc">{$t('liveEvents.form.description')} <span class="font-normal text-muted-foreground">{$t('common.optional')}</span></Label>
				<Textarea id="ls-desc" rows={2} bind:value={sForm.description} />
			</div>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				{@render kindField('ls-kind', sForm.kind, (v) => (sForm.kind = v), false)}
				<div class="space-y-1.5">
					<Label for="ls-tz">{$t('liveEvents.series.timezone')}</Label>
					<Combobox items={timezoneOptions(sForm.timezone)} bind:value={sForm.timezone} placeholder={$t('liveEvents.series.timezone')} />
					<p class="text-xs text-muted-foreground">{$t('liveEvents.series.timezoneHint')}</p>
				</div>
			</div>

			<div class="space-y-1.5">
				<p class="text-sm font-medium">{$t('liveEvents.series.weekdays')}</p>
				<div class="flex flex-wrap gap-1.5" role="group" aria-label={$t('liveEvents.series.weekdays')}>
					{#each WEEK_ORDER as d}
						<Button
							size="sm"
							variant={sForm.weekdays.includes(d) ? 'default' : 'outline'}
							aria-pressed={sForm.weekdays.includes(d)}
							onclick={() => toggleDay(d)}>{dayNames[d]}</Button
						>
					{/each}
				</div>
			</div>
			<div class="grid grid-cols-2 gap-4 sm:grid-cols-3">
				<div class="space-y-1.5">
					<Label for="ls-time">{$t('liveEvents.series.startTime')}</Label>
					<Input id="ls-time" type="time" bind:value={sForm.start_time} />
				</div>
				<div class="space-y-1.5">
					<Label for="ls-dur">{$t('liveEvents.series.duration')}</Label>
					<Input id="ls-dur" type="number" min="1" max="1440" bind:value={sForm.duration_minutes} />
				</div>
				<div class="space-y-1.5">
					<Label for="ls-every">{$t('liveEvents.series.interval')}</Label>
					<Input id="ls-every" type="number" min="1" max="52" bind:value={sForm.interval_weeks} />
				</div>
			</div>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div class="space-y-1.5">
					<Label>{$t('liveEvents.series.startDate')}</Label>
					<DatePicker bind:value={sForm.start_date} placeholder={$t('liveEvents.form.date')} class="w-full" />
				</div>
				<div class="space-y-1.5">
					<Label>{$t('liveEvents.series.endDate')} <span class="font-normal text-muted-foreground">{$t('common.optional')}</span></Label>
					<div class="flex gap-2">
						<DatePicker bind:value={sForm.end_date} placeholder={$t('liveEvents.series.noEnd')} class="w-full" />
						{#if sForm.end_date}<Button variant="ghost" size="sm" onclick={() => (sForm.end_date = '')}>{$t('common.clear')}</Button>{/if}
					</div>
				</div>
			</div>

			<div class="space-y-1.5">
				<p class="text-sm font-medium">{$t('liveEvents.series.skip')}</p>
				<p class="text-xs text-muted-foreground">{$t('liveEvents.series.skipHint')}</p>
				<div class="flex flex-wrap items-center gap-2">
					<DatePicker bind:value={skipFrom} placeholder={$t('liveEvents.series.skipFrom')} class="w-[150px]" />
					<DatePicker bind:value={skipTo} placeholder={$t('liveEvents.series.skipTo')} class="w-[150px]" />
					<Button variant="outline" size="sm" disabled={!skipFrom} onclick={addSkip}>{$t('liveEvents.series.skipAdd')}</Button>
				</div>
				{#if sForm.skip_dates.length > 0}
					<div class="flex flex-wrap gap-1.5">
						{#each sForm.skip_dates as s (s)}
							<Badge variant="secondary" class="gap-1">
								{skipLabel(s)}
								<button
									type="button"
									class="ml-0.5 rounded-sm opacity-70 hover:opacity-100"
									aria-label={$t('common.remove')}
									onclick={() => (sForm.skip_dates = sForm.skip_dates.filter((x) => x !== s))}>×</button
								>
							</Badge>
						{/each}
					</div>
				{/if}
			</div>

			{#if $currentUser?.is_admin && members.length > 0}
				<div class="space-y-1.5">
					<p class="text-sm font-medium">{$t('liveEvents.series.hosts')}</p>
					<p class="text-xs text-muted-foreground">{$t('liveEvents.series.hostsHint')}</p>
					<div class="grid max-h-40 grid-cols-1 gap-1 overflow-y-auto rounded-md border p-2 sm:grid-cols-2">
						{#each members as m (m.id)}
							<label class="flex items-center gap-2 text-sm">
								<Checkbox checked={sForm.host_user_ids.includes(m.id)} onCheckedChange={() => toggleHost(m.id)} />
								<span class="truncate">{m.name}</span>
								{#if sForm.host_user_ids.includes(m.id) && sForm.host_user_ids.length > 1}
									<Badge variant="outline" class="ml-auto text-[10px]">{sForm.host_user_ids.indexOf(m.id) + 1}</Badge>
								{/if}
							</label>
						{/each}
					</div>
				</div>
			{/if}

			<div class="space-y-1.5">
				<p class="text-sm font-medium">{$t('liveEvents.series.link')}</p>
				<div class="flex flex-col gap-2 text-sm">
					<label class="flex items-start gap-2">
						<input type="radio" class="mt-1" name="ls-link" value="fresh" bind:group={sForm.link_mode} />
						<span>{$t('liveEvents.series.linkFresh')}<span class="block text-xs text-muted-foreground">{$t('liveEvents.series.linkFreshHint')}</span></span>
					</label>
					<label class="flex items-start gap-2">
						<input type="radio" class="mt-1" name="ls-link" value="fixed" bind:group={sForm.link_mode} />
						<span>{$t('liveEvents.series.linkFixed')}<span class="block text-xs text-muted-foreground">{$t('liveEvents.series.linkFixedHint')}</span></span>
					</label>
				</div>
				{#if sForm.link_mode === 'fixed'}
					<Input type="url" bind:value={sForm.join_url} placeholder="https://meet.google.com/…" aria-label={$t('liveEvents.form.joinLink')} />
				{/if}
			</div>

			<div class="flex flex-wrap gap-6">
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={sForm.auto_start} />
					{$t('liveEvents.form.autoStart')}
				</label>
				<label class="flex items-center gap-2 text-sm">
					<Switch bind:checked={sForm.auto_end} />
					{$t('liveEvents.form.autoEnd')}
				</label>
			</div>
			{@render inviteSwitch(sForm.invite_default_participants, (v) => (sForm.invite_default_participants = v))}
		</div>

		<Dialog.Footer class="mt-2">
			<Button variant="outline" onclick={() => (seriesOpen = false)} disabled={seriesSaving}>{$t('common.cancel')}</Button>
			<Button onclick={saveSeries} disabled={seriesSaving}>
				{seriesSaving ? $t('common.saving') : seriesTarget ? $t('liveEvents.edit.save') : $t('liveEvents.series.create')}
			</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<div class="mb-8 flex flex-wrap items-center justify-between gap-3">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">{$t('liveEvents.title')}</h1>
		<p class="mt-1 text-sm text-muted-foreground">{$t('liveEvents.subtitle')}</p>
	</div>
	<div class="flex gap-2">
		<Button variant="outline" onclick={() => openSeries(null)}>{$t('liveEvents.series.new')}</Button>
		<Button onclick={openCreate}>{$t('liveEvents.new')}</Button>
	</div>
</div>

{#if loading}
	<p class="text-sm text-muted-foreground">{$t('common.loading')}</p>
{:else}
	{#if series.length > 0}
		<div class="mb-8 rounded-lg border bg-card">
			<div class="border-b p-4">
				<h2 class="text-sm font-semibold">{$t('liveEvents.series.title')}</h2>
				<p class="mt-0.5 text-xs text-muted-foreground">{$t('liveEvents.series.subtitle')}</p>
			</div>
			<ul>
				{#each series as s (s.id)}
					<li class="flex flex-wrap items-center gap-3 border-b px-4 py-3 last:border-0">
						<div class="min-w-0 flex-1">
							<p class="font-medium">{s.title} <span class="text-xs font-normal text-muted-foreground">· {kindLabel(s.kind)}</span></p>
							<p class="text-xs text-muted-foreground">{seriesSummary(s)}</p>
							<p class="text-xs text-muted-foreground">
								{seriesHosts(s)} · {s.join_url ? $t('liveEvents.series.linkFixedShort') : $t('liveEvents.series.linkFreshShort')}
								{#if !s.invite_default_participants}· {$t('liveEvents.series.noDefaultParticipants')}{/if}
								{#if s.skip_dates.length > 0}· {$t('liveEvents.series.skipCount', { count: s.skip_dates.length })}{/if}
							</p>
						</div>
						{#if canManageSeries(s)}
							<Tooltip.Provider>
								<div class="flex items-center gap-1">
									<Tooltip.Root>
										<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => openSeries(s)} aria-label={$t('liveEvents.series.editTitle')}>
											<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>
										</Tooltip.Trigger>
										<Tooltip.Content>{$t('liveEvents.series.editTitle')}</Tooltip.Content>
									</Tooltip.Root>
									<Tooltip.Root>
										<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => askEndSeries(s)} aria-label={$t('liveEvents.series.confirmEnd.action')}>
											<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-destructive"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
										</Tooltip.Trigger>
										<Tooltip.Content>{$t('liveEvents.series.confirmEnd.action')}</Tooltip.Content>
									</Tooltip.Root>
								</div>
							</Tooltip.Provider>
						{/if}
					</li>
				{/each}
			</ul>
		</div>
	{/if}

	<div class="mb-3 flex items-center justify-between">
		<p class="text-sm text-muted-foreground">
			{#if liveCount > 0}
				<span class="font-medium text-foreground">{$t('liveEvents.liveNowCount', { count: liveCount })}</span> ·
			{/if}
			{$t('liveEvents.shownCount', { count: visible.length })}
		</p>
	</div>

	{#if visible.length === 0}
		<div class="rounded-lg border border-dashed p-10 text-center">
			<p class="text-sm text-muted-foreground">{$t('liveEvents.empty')}</p>
		</div>
	{:else}
		<div class="overflow-x-auto rounded-lg border bg-card">
			<table class="w-full text-sm">
				<thead class="border-b bg-muted/50 text-left text-xs uppercase tracking-wide text-muted-foreground">
					<tr>
						<th class="px-4 py-3">{$t('liveEvents.table.status')}</th>
						<th class="px-4 py-3">{$t('liveEvents.table.session')}</th>
						<th class="px-4 py-3">{$t('liveEvents.table.host')}</th>
						<th class="px-4 py-3">{$t('liveEvents.table.when')}</th>
						<th class="px-4 py-3">{$t('liveEvents.table.joinLink')}</th>
						<th class="px-4 py-3 text-right">{$t('liveEvents.table.actions')}</th>
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
									{/if}
									{statusLabel(e.status)}
								</Badge>
							</td>
							<td class="px-4 py-3">
								<p class="font-medium">
									{e.title}
									{#if e.series_id}<Badge variant="outline" class="ml-1 text-[10px]">{$t('liveEvents.recurringBadge')}</Badge>{/if}
								</p>
								<p class="text-xs text-muted-foreground">
									{kindLabel(e.kind)}{e.auto_start && e.status === 'scheduled' && e.scheduled_start_at ? ` · ${$t('liveEvents.autoStartShort')}` : ''}
								</p>
							</td>
							<td class="px-4 py-3">{e.host_name || $t('common.emDash')}</td>
							<td class="px-4 py-3">{schedule(e)}</td>
							<td class="px-4 py-3">
								{#if e.join_url}
									<Tooltip.Provider>
										<div class="flex items-center gap-1">
											<Tooltip.Root>
												<Tooltip.Trigger
													class={buttonVariants({ variant: 'ghost', size: 'icon' })}
													onclick={() => copy(e.join_url, tr('liveEvents.table.joinLink'))}
												>
													<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="8" y="8" width="14" height="14" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>
												</Tooltip.Trigger>
												<Tooltip.Content>{$t('liveEvents.tooltip.copyLink')}</Tooltip.Content>
											</Tooltip.Root>
											<span class="max-w-[180px] truncate text-xs text-muted-foreground">{e.join_url}</span>
										</div>
									</Tooltip.Provider>
								{:else if e.status === 'scheduled' && e.has_calendar_event}
									<span class="text-xs text-muted-foreground">{$t('liveEvents.link.mintedAtStart')}</span>
								{:else if e.status === 'scheduled'}
									<!-- No calendar event means nothing will mint a link: say so instead of promising one. -->
									<span class="text-xs text-muted-foreground">{$t('liveEvents.link.none')}</span>
								{:else}
									<span class="text-xs text-muted-foreground">{$t('common.emDash')}</span>
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
												<Tooltip.Content>{$t('liveEvents.tooltip.edit')}</Tooltip.Content>
											</Tooltip.Root>
											{#if e.status === 'scheduled'}
												<Tooltip.Root>
													<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => start(e)}>
														<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="5 3 19 12 5 21 5 3"/></svg>
													</Tooltip.Trigger>
													<Tooltip.Content>{$t('liveEvents.tooltip.startNow')}</Tooltip.Content>
												</Tooltip.Root>
											{:else}
												<Tooltip.Root>
													<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => askEnd(e)}>
														<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="5" width="14" height="14" rx="2"/></svg>
													</Tooltip.Trigger>
													<Tooltip.Content>{$t('liveEvents.tooltip.end')}</Tooltip.Content>
												</Tooltip.Root>
											{/if}
											<Tooltip.Root>
												<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => askCancel(e)}>
													<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-destructive"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>
												</Tooltip.Trigger>
												<Tooltip.Content>{$t('liveEvents.tooltip.cancel')}</Tooltip.Content>
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
				<h2 class="text-sm font-semibold">{$t('liveEvents.history.title')}</h2>
				<p class="mt-0.5 text-xs text-muted-foreground">{$t('liveEvents.history.subtitle')}</p>
			</div>
			<Select.Root type="single" value={historyKind} onValueChange={(v) => { historyKind = v ?? ''; loadHistory(true); }}>
				<Select.Trigger class="w-48" aria-label={$t('liveEvents.history.filterLabel')}>{historyKind ? kindLabel(historyKind) : $t('liveEvents.history.allKinds')}</Select.Trigger>
				<Select.Content>
					<Select.Item value="" label={$t('liveEvents.history.allKinds')}>{$t('liveEvents.history.allKinds')}</Select.Item>
					{#each knownKinds as k}<Select.Item value={k} label={kindLabel(k)}>{kindLabel(k)}</Select.Item>{/each}
				</Select.Content>
			</Select.Root>
		</div>
		{#if history.length === 0}
			<p class="p-6 text-center text-sm text-muted-foreground">{historyLoading ? $t('common.loading') : $t('liveEvents.history.empty')}</p>
		{:else}
			<div class="overflow-x-auto">
				<table class="w-full text-sm">
					<thead class="border-b bg-muted/50 text-left text-xs uppercase tracking-wide text-muted-foreground">
						<tr>
							<th class="px-4 py-3">{$t('liveEvents.table.session')}</th>
							<th class="px-4 py-3">{$t('liveEvents.table.host')}</th>
							<th class="px-4 py-3">{$t('liveEvents.history.started')}</th>
							<th class="px-4 py-3">{$t('liveEvents.history.duration')}</th>
							<th class="px-4 py-3">{$t('liveEvents.history.outcome')}</th>
						</tr>
					</thead>
					<tbody>
						{#each history as e (e.id)}
							<tr class="border-b last:border-0">
								<td class="px-4 py-3">
									<p class="font-medium">{e.title}</p>
									<p class="text-xs text-muted-foreground">{kindLabel(e.kind)}</p>
								</td>
								<td class="px-4 py-3">{e.host_name || $t('common.emDash')}</td>
								<td class="px-4 py-3">
									{e.started_at
										? fmtWhen(e.started_at)
										: e.scheduled_start_at
											? $t('liveEvents.history.scheduledAt', { when: fmtWhen(e.scheduled_start_at) })
											: $t('common.emDash')}
								</td>
								<td class="px-4 py-3">{duration(e)}</td>
								<td class="px-4 py-3">
									<Badge variant={badgeVariant(e.status)}>{statusLabel(e.status)}</Badge>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
			{#if historyMore}
				<div class="border-t p-3 text-center">
					<Button variant="ghost" size="sm" disabled={historyLoading} onclick={() => loadHistory(false)}>
						{historyLoading ? $t('common.loading') : $t('liveEvents.history.loadMore')}
					</Button>
				</div>
			{/if}
		{/if}
	</div>

	<div class="mt-8 rounded-lg border bg-card p-6">
		<div class="mb-4 flex items-start justify-between gap-4">
			<div>
				<h2 class="text-sm font-semibold">{$t('liveEvents.embed.title')}</h2>
				<p class="mt-1 text-sm text-muted-foreground">{$t('liveEvents.embed.subtitle')}</p>
			</div>
			<a href={livePageURL} target="_blank" rel="noopener noreferrer" class={buttonVariants({ variant: 'outline', size: 'sm' })}>
				{$t('liveEvents.embed.openPage')}
			</a>
		</div>
		<div class="space-y-4">
			<div class="space-y-1.5">
				<Label for="embed-kind">{$t('liveEvents.embed.kindLabel')}</Label>
				<Select.Root type="single" value={embedKind} onValueChange={(v) => (embedKind = v ?? '')}>
					<Select.Trigger id="embed-kind" class="w-full sm:w-64">{embedKind ? kindLabel(embedKind) : $t('liveEvents.embed.everyKind')}</Select.Trigger>
					<Select.Content>
						<Select.Item value="" label={$t('liveEvents.embed.everyKind')}>{$t('liveEvents.embed.everyKind')}</Select.Item>
						{#each knownKinds as k}<Select.Item value={k} label={kindLabel(k)}>{kindLabel(k)}</Select.Item>{/each}
					</Select.Content>
				</Select.Root>
				<p class="text-xs text-muted-foreground">{$t('liveEvents.embed.kindHint')}</p>
			</div>
			<div class="space-y-1.5">
				<div class="flex items-center justify-between">
					<Label>{$t('liveEvents.embed.iframe')}</Label>
					<Button variant="ghost" size="sm" onclick={() => copy(iframeSnippet, tr('liveEvents.embed.iframe'))}>{$t('common.copy')}</Button>
				</div>
				<pre class="overflow-x-auto rounded-md bg-muted p-3 text-xs"><code>{iframeSnippet}</code></pre>
				<p class="text-xs text-muted-foreground">{$t('liveEvents.embed.darkHint')}</p>
			</div>
			<div class="space-y-1.5">
				<div class="flex items-center justify-between">
					<Label>{$t('liveEvents.embed.widget')}</Label>
					<Button variant="ghost" size="sm" onclick={() => copy(widgetSnippet, tr('liveEvents.embed.widget'))}>{$t('common.copy')}</Button>
				</div>
				<pre class="overflow-x-auto rounded-md bg-muted p-3 text-xs"><code>{widgetSnippet}</code></pre>
				<p class="text-xs text-muted-foreground">{$t('liveEvents.embed.widgetHint')}</p>
			</div>
			<p class="text-xs text-muted-foreground">{$t('liveEvents.embed.rawStatus', { url: `GET ${origin}/v1/live/status` })}</p>
		</div>
	</div>
{/if}
