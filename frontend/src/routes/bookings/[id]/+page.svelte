<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { base } from '$app/paths';
	import { api } from '$lib/api';
	import { Badge } from '$lib/components/ui/badge';
	import { buttonVariants } from '$lib/components/ui/button';

	// The page webhook payloads link to as admin_url: one booking, read-only, in the
	// full shape GET /v1/bookings/{id} returns (the same data the webhooks carry).
	type Full = {
		id: string;
		event_type_slug?: string;
		event_type_name?: string;
		status: string;
		start_at: string;
		end_at: string;
		cancellation_reason?: string;
		created_at: string;
		revision?: number;
		changed_at?: string;
		meeting?: { provider: string; join_url: string; calendar_provider: string; calendar_event_id: string; ical_uid: string };
		hosts?: { id: string; name: string; email?: string; role?: string }[];
		attendees?: { name: string; email: string; timezone?: string; phone?: string; rsvp_status?: string; organizer?: boolean }[];
		answers?: { question_id: string; question: string; answer: string }[];
	};

	const id = $page.params.id;
	let b = $state<Full | null>(null);
	let error = $state('');

	onMount(async () => {
		try {
			b = await api.get<Full>(`/v1/bookings/${encodeURIComponent(id ?? '')}`);
		} catch (e: any) {
			error = e.message || 'Could not load this booking';
		}
	});

	function when(iso: string) {
		return new Date(iso).toLocaleString(undefined, { dateStyle: 'full', timeStyle: 'short' });
	}
	const providerLabel: Record<string, string> = {
		google_meet: 'Google Meet', teams: 'Microsoft Teams', zoom: 'Zoom', livekit: 'Calnode video',
		phone: 'Phone call', in_person: 'In person', manual: 'Link'
	};
</script>

<svelte:head><title>Booking — Calnode</title></svelte:head>

<a href="{base}/bookings" class="mb-4 inline-block text-sm text-muted-foreground hover:text-foreground">← Bookings</a>

{#if error}
	<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</p>
{:else if !b}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{:else}
	<div class="mb-6 flex flex-wrap items-start justify-between gap-3">
		<div>
			<h1 class="text-2xl font-semibold tracking-tight">{b.event_type_name ?? 'Booking'}</h1>
			<p class="mt-1 text-sm text-muted-foreground">{when(b.start_at)} – {new Date(b.end_at).toLocaleTimeString(undefined, { timeStyle: 'short' })}</p>
		</div>
		<Badge variant={b.status === 'cancelled' ? 'outline' : 'secondary'}>{b.status}</Badge>
	</div>

	{#if b.status === 'cancelled' && b.cancellation_reason}
		<p class="mb-6 text-sm"><span class="text-muted-foreground">Cancellation reason:</span> {b.cancellation_reason}</p>
	{/if}

	<div class="grid gap-4 md:grid-cols-2">
		<section class="rounded-lg border bg-card p-5">
			<h2 class="mb-3 text-sm font-semibold uppercase tracking-wider text-muted-foreground">Meeting</h2>
			{#if b.meeting}
				<p class="text-sm">{providerLabel[b.meeting.provider] ?? b.meeting.provider}</p>
				{#if b.meeting.join_url}
					<a href={b.meeting.join_url} target="_blank" rel="noopener noreferrer"
						class="{buttonVariants({ variant: 'outline', size: 'sm' })} mt-2 max-w-full truncate">Join link</a>
				{/if}
				<dl class="mt-3 space-y-1 text-xs text-muted-foreground">
					<div>Calendar: {b.meeting.calendar_provider}</div>
					{#if b.meeting.ical_uid}<div class="break-all">iCalUID: <span class="font-mono">{b.meeting.ical_uid}</span></div>{/if}
				</dl>
			{/if}
		</section>

		<section class="rounded-lg border bg-card p-5">
			<h2 class="mb-3 text-sm font-semibold uppercase tracking-wider text-muted-foreground">Hosts</h2>
			<ul class="space-y-2">
				{#each b.hosts ?? [] as h}
					<li class="text-sm">
						<span class="font-medium">{h.name}</span>
						{#if h.role}<Badge variant="outline" class="ml-1.5 text-[10px]">{h.role}</Badge>{/if}
						{#if h.email}<span class="block text-xs text-muted-foreground">{h.email}</span>{/if}
					</li>
				{/each}
			</ul>
		</section>

		<section class="rounded-lg border bg-card p-5">
			<h2 class="mb-3 text-sm font-semibold uppercase tracking-wider text-muted-foreground">Attendees</h2>
			<ul class="space-y-2">
				{#each b.attendees ?? [] as a}
					<li class="text-sm">
						<span class="font-medium">{a.name}</span>
						{#if a.organizer}<Badge variant="outline" class="ml-1.5 text-[10px]">booker</Badge>{/if}
						{#if a.rsvp_status && a.rsvp_status !== 'needs-action'}<Badge variant="secondary" class="ml-1.5 text-[10px]">{a.rsvp_status}</Badge>{/if}
						<span class="block text-xs text-muted-foreground">{[a.email, a.timezone, a.phone].filter(Boolean).join(' · ')}</span>
					</li>
				{/each}
			</ul>
		</section>

		<section class="rounded-lg border bg-card p-5">
			<h2 class="mb-3 text-sm font-semibold uppercase tracking-wider text-muted-foreground">Answers</h2>
			{#if (b.answers ?? []).length === 0}
				<p class="text-sm text-muted-foreground">No intake answers.</p>
			{:else}
				<dl class="space-y-2">
					{#each b.answers ?? [] as q}
						<div class="text-sm"><dt class="text-xs text-muted-foreground">{q.question}</dt><dd class="whitespace-pre-line">{q.answer}</dd></div>
					{/each}
				</dl>
			{/if}
		</section>
	</div>

	<p class="mt-6 text-xs text-muted-foreground">
		Booking <span class="font-mono">{b.id}</span> · revision {b.revision ?? '—'} · created {new Date(b.created_at).toLocaleString()}
	</p>
{/if}
