<script lang="ts">
	import { onMount } from 'svelte';
	import { api, type Webhook, type WebhookDelivery } from '$lib/api';
	import { Button, buttonVariants } from '$lib/components/ui/button';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Checkbox } from '$lib/components/ui/checkbox';
	import { Badge } from '$lib/components/ui/badge';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { currentUser } from '$lib/stores';
	import * as Dialog from '$lib/components/ui/dialog';

	let items: Webhook[] = $state([]);
	let loading = $state(true);
	let error = $state('');
	let showCreate = $state(false);

	const allEvents = [
		'booking.created',
		'booking.cancelled',
		'booking.rescheduled',
		'booking.reassigned',
		'booking.updated',
		'booking.rsvp',
		'recording.completed',
		'transcript.ready',
		'notes.ready'
	];

	// Payload field catalog (keys must match the backend's webhook field keys).
	// `pii` flags personal data so the operator chooses consciously what leaves the system.
	type FieldDef = { key: string; label: string; pii?: boolean };
	const fieldGroups: { group: string; pii?: boolean; fields: FieldDef[] }[] = [
		{ group: 'Booking', fields: [
			{ key: 'id', label: 'Booking reference' },
			{ key: 'status', label: 'Status' },
			{ key: 'start_at', label: 'Start time' },
			{ key: 'end_at', label: 'End time' },
			{ key: 'created_at', label: 'Created at' },
			{ key: 'location_value', label: 'Location' },
			{ key: 'cancellation_reason', label: 'Cancellation reason' },
			{ key: 'previous_start_at', label: 'Previous start (reschedule)' },
			{ key: 'previous_end_at', label: 'Previous end (reschedule)' },
			{ key: 'rsvp_status', label: 'RSVP answer (booking.rsvp)' },
			{ key: 'initiated_by', label: 'Initiated by (booker / host / admin / system)' },
			{ key: 'revision', label: 'Revision (orders events)' },
			{ key: 'occurred_at', label: 'Occurred at' },
			{ key: 'changed', label: 'Changed fields (booking.updated)' },
			{ key: 'admin_url', label: 'Admin link' },
			{ key: 'meeting', label: 'Meeting (link, calendar event, iCalUID)' },
		] },
		{ group: 'Payment', fields: [
			{ key: 'payment_status', label: 'Payment status' },
			{ key: 'amount_paid_cents', label: 'Amount paid (cents)' },
			{ key: 'amount_paid_currency', label: 'Currency' },
		] },
		{ group: 'Event type', fields: [
			{ key: 'event_type_slug', label: 'Event type slug' },
			{ key: 'event_type_name', label: 'Event type name' },
		] },
		{ group: 'Host', fields: [
			{ key: 'host_id', label: 'Host ID' },
			{ key: 'host_name', label: 'Host name' },
			{ key: 'host_email', label: 'Host email', pii: true },
			{ key: 'hosts', label: 'All hosts', pii: true },
			{ key: 'previous_host_id', label: 'Previous host ID (reassigned)' },
			{ key: 'previous_host_name', label: 'Previous host name (reassigned)' },
			{ key: 'previous_host_email', label: 'Previous host email (reassigned)', pii: true },
		] },
		{ group: 'Attendee', pii: true, fields: [
			{ key: 'attendee_name', label: 'Attendee name', pii: true },
			{ key: 'attendee_email', label: 'Attendee email', pii: true },
			{ key: 'attendee_timezone', label: 'Attendee timezone', pii: true },
			{ key: 'attendees', label: 'All attendees (incl. phone)', pii: true },
		] },
		{ group: 'Intake', pii: true, fields: [
			{ key: 'answers', label: 'Intake answers', pii: true },
		] },
	];
	const allFieldKeys = fieldGroups.flatMap((g) => g.fields.map((f) => f.key));

	// org = every booking in the workspace, not only the ones you host (admins only).
	const blankForm = () => ({
		url: '',
		events: ['booking.created', 'booking.cancelled', 'booking.rescheduled', 'booking.reassigned', 'booking.updated'],
		fields: [...allFieldKeys], org: false, eventTypes: [] as string[]
	});
	let form = $state<{ url: string; events: string[]; fields: string[]; org: boolean; eventTypes: string[] }>(blankForm());

	// Event types the filter can pick from. None ticked = every event type.
	let eventTypeOptions = $state<{ slug: string; name: string }[]>([]);

	// The signing secret is shown once: after create, and after a rotation.
	let secretOpen = $state(false);
	let secretValue = $state('');
	let secretRotated = $state(false);
	let secretCopied = $state(false);
	function showSecret(secret: string, rotated: boolean) {
		secretValue = secret; secretRotated = rotated; secretCopied = false; secretOpen = true;
	}
	function copySecret() {
		navigator.clipboard.writeText(secretValue).then(() => (secretCopied = true)).catch(() => {});
	}

	let rotateOpen = $state(false);
	let rotateId = $state('');
	async function doRotate() {
		try {
			const res = await api.post<{ secret: string }>(`/v1/webhooks/${rotateId}/rotate-secret`);
			showSecret(res.secret, true);
			await load();
		} catch (e: any) {
			error = e.message;
		}
	}

	let redelivering = $state<string | null>(null);
	async function redeliver(webhookId: string, deliveryId: string) {
		redelivering = deliveryId;
		try {
			await api.post(`/v1/webhooks/${webhookId}/deliveries/${deliveryId}/redeliver`);
			const res = await api.get<{ items: WebhookDelivery[] }>(`/v1/webhooks/${webhookId}/deliveries`);
			deliveries = res.items ?? [];
		} catch (e: any) {
			error = e.message;
		} finally {
			redelivering = null;
		}
	}

	// Delivery log (lazy-loaded per webhook).
	let openDeliveries = $state<string | null>(null);
	let deliveries = $state<WebhookDelivery[]>([]);
	let deliveriesLoading = $state(false);
	let creating = $state(false);
	let createError = $state('');
	let deleteOpen = $state(false);
	let deleteId = $state('');

	async function load() {
		try {
			const res = await api.get<{ items: Webhook[] }>('/v1/webhooks');
			items = res.items;
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		load();
		api.get<{ items: { slug: string; name: string }[] }>('/v1/event-types')
			.then((r) => (eventTypeOptions = r.items ?? []))
			.catch(() => {});
	});

	async function create() {
		createError = '';
		if (!form.url) { createError = 'URL is required.'; return; }
		if (!form.url.startsWith('https://')) { createError = 'URL must start with https://'; return; }
		if (form.events.length === 0) { createError = 'Select at least one event.'; return; }
		creating = true;
		try {
			const res = await api.post<{ secret: string }>('/v1/webhooks', {
				url: form.url, events: form.events, fields: form.fields, scope: form.org ? 'org' : 'user',
				event_types: form.eventTypes
			});
			form = blankForm();
			showSecret(res.secret, false);
			showCreate = false;
			await load();
		} catch (e: any) {
			createError = e.message;
		} finally {
			creating = false;
		}
	}

	function del(id: string) {
		deleteId = id;
		deleteOpen = true;
	}

	async function doDelete() {
		try {
			await api.del(`/v1/webhooks/${deleteId}`);
			await load();
		} catch (e: any) {
			error = e.message;
		}
	}

	function fmtDate(iso: string) {
		return new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' });
	}

	function toggleEvent(ev: string) {
		if (form.events.includes(ev)) {
			form.events = form.events.filter((e) => e !== ev);
		} else {
			form.events = [...form.events, ev];
		}
	}

	function toggleField(key: string) {
		form.fields = form.fields.includes(key)
			? form.fields.filter((f) => f !== key)
			: [...form.fields, key];
	}

	async function toggleDeliveries(id: string) {
		if (openDeliveries === id) { openDeliveries = null; return; }
		openDeliveries = id;
		deliveries = [];
		deliveriesLoading = true;
		try {
			const res = await api.get<{ items: WebhookDelivery[] }>(`/v1/webhooks/${id}/deliveries`);
			deliveries = res.items ?? [];
		} catch (e: any) {
			error = e.message;
		} finally {
			deliveriesLoading = false;
		}
	}
</script>

<ConfirmDialog
	bind:open={deleteOpen}
	title="Delete webhook?"
	description="The endpoint will stop receiving events immediately."
	confirmText="Delete"
	destructive
	onConfirm={doDelete}
/>

<ConfirmDialog
	bind:open={rotateOpen}
	title="Rotate the signing secret?"
	description="You get a new secret, shown once. For the next 24 hours every delivery is signed with both the new and the old secret, so you can update your receiver without rejecting anything."
	confirmText="Rotate secret"
	onConfirm={doRotate}
/>

<Dialog.Root bind:open={secretOpen}>
	<Dialog.Content class="sm:max-w-lg">
		<Dialog.Header>
			<Dialog.Title>{secretRotated ? 'New signing secret' : 'Webhook created'}</Dialog.Title>
			<Dialog.Description>
				Copy the signing secret now. You will not see it again.
				{#if secretRotated}The old secret keeps signing alongside it for 24 hours.{/if}
			</Dialog.Description>
		</Dialog.Header>
		<div class="rounded-md border bg-muted/40 px-3 py-2 font-mono text-xs break-all">{secretValue}</div>
		<p class="text-xs text-muted-foreground">
			Verify <span class="font-mono">X-Calnode-Signature</span> as HMAC-SHA256 of the raw request body, keyed by
			the <strong>hex-decoded</strong> secret (32 bytes), not the text above. During a rotation the header holds
			two values, comma-separated; accept the delivery if either matches.
		</p>
		<Dialog.Footer>
			<Button variant="outline" onclick={copySecret}>{secretCopied ? 'Copied' : 'Copy secret'}</Button>
			<Button onclick={() => (secretOpen = false)}>Done</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<svelte:head><title>Webhooks — Calnode</title></svelte:head>

<div class="mb-8 flex items-center justify-between">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">Webhooks</h1>
		<p class="mt-1 text-sm text-muted-foreground">Receive real-time notifications for booking events.</p>
	</div>
	<Button onclick={() => { showCreate = !showCreate; createError = ''; }}>
		{showCreate ? 'Cancel' : 'New webhook'}
	</Button>
</div>

{#if showCreate}
	<div class="mb-6 rounded-lg border bg-card p-6">
		<h2 class="mb-4 text-sm font-semibold">New webhook</h2>
		{#if createError}<p class="mb-3 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{createError}</p>{/if}

		<div class="mb-4 space-y-1.5">
			<Label for="wh-url">Endpoint URL</Label>
			<Input
				id="wh-url"
				type="url"
				bind:value={form.url}
				placeholder="https://your-server.com/hooks/calnode"
			/>
		</div>

		{#if $currentUser?.is_admin}
			<div class="mb-4 space-y-1">
				<label class="flex cursor-pointer items-center gap-2 text-sm font-medium">
					<Checkbox checked={form.org} onCheckedChange={(v) => (form.org = v === true)} />
					Every booking in the workspace
				</label>
				<p class="pl-6 text-xs text-muted-foreground">
					{form.org
						? 'Organisation-wide: fires for every member\'s bookings. Any admin can see and remove it.'
						: 'Only bookings you host. Tick this for an integration that should hear about the whole team.'}
				</p>
			</div>
		{/if}

		<div class="mb-4 space-y-2">
			<p class="text-sm font-medium">Events to send</p>
			{#each allEvents as ev}
				<label class="flex cursor-pointer items-center gap-2 font-mono text-sm">
					<Checkbox
						checked={form.events.includes(ev)}
						onCheckedChange={() => toggleEvent(ev)}
					/>
					<span>{ev}</span>
				</label>
			{/each}
		</div>

		{#if eventTypeOptions.length > 0}
			<div class="mb-4 space-y-2">
				<p class="text-sm font-medium">Event types <span class="font-normal text-muted-foreground">— none ticked sends every event type</span></p>
				<div class="grid grid-cols-2 gap-x-4 gap-y-1">
					{#each eventTypeOptions as et}
						<label class="flex cursor-pointer items-center gap-2 text-sm">
							<Checkbox checked={form.eventTypes.includes(et.slug)}
								onCheckedChange={(v) => (form.eventTypes = v === true ? [...form.eventTypes, et.slug] : form.eventTypes.filter((x) => x !== et.slug))} />
							<span>{et.name} <span class="font-mono text-xs text-muted-foreground">{et.slug}</span></span>
						</label>
					{/each}
				</div>
			</div>
		{/if}

		<div class="mb-4 space-y-3">
			<p class="text-sm font-medium">Data to send <span class="font-normal text-muted-foreground">— untick anything you don't want delivered</span></p>
			{#each fieldGroups as grp}
				<div class="space-y-1.5">
					<p class="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
						{grp.group}{#if grp.pii}<span class="ml-1.5 font-normal normal-case text-amber-600">· personal data</span>{/if}
					</p>
					<div class="grid grid-cols-2 gap-x-4 gap-y-1">
						{#each grp.fields as f}
							<label class="flex cursor-pointer items-center gap-2 font-mono text-sm">
								<Checkbox checked={form.fields.includes(f.key)} onCheckedChange={() => toggleField(f.key)} />
								<span>{f.label}{#if f.pii}<span class="ml-1 text-[10px] font-medium uppercase text-amber-600">PII</span>{/if}</span>
							</label>
						{/each}
					</div>
				</div>
			{/each}
		</div>

		<Button onclick={create} disabled={creating}>
			{creating ? 'Creating…' : 'Create webhook'}
		</Button>
	</div>
{/if}

{#if error}<p class="mb-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</p>{/if}

{#if loading}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{:else if items.length === 0}
	<div class="rounded-lg border border-dashed bg-card p-12 text-center">
		<p class="text-sm font-medium">No webhooks</p>
		<p class="mt-1 text-sm text-muted-foreground">Add a webhook to receive real-time notifications for booking events.</p>
	</div>
{:else}
	<div class="rounded-lg border bg-card overflow-hidden">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b">
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">URL</th>
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">Events</th>
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">Fields</th>
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">Status</th>
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">Created</th>
					<th class="px-4 pb-3 pt-3"></th>
				</tr>
			</thead>
			<tbody class="divide-y">
				<Tooltip.Provider>
					{#each items as wh}
						<tr class="transition-colors hover:bg-muted/30">
							<td class="max-w-xs overflow-hidden text-ellipsis whitespace-nowrap px-4 py-3 font-mono text-xs">
								{#if wh.scope === 'org'}<Badge variant="secondary" class="mr-1.5 font-sans">Workspace</Badge>{/if}{wh.url}
							</td>
							<td class="px-4 py-3 text-xs text-muted-foreground">
								{(wh.events ?? []).join(', ')}
								{#if wh.event_types && wh.event_types.length > 0}
									<span class="mt-1 block">Only: <span class="font-mono">{wh.event_types.join(', ')}</span></span>
								{/if}
								{#if wh.previous_secret_valid_until}
									<span class="mt-1 block text-amber-700 dark:text-amber-300">Old secret valid until {new Date(wh.previous_secret_valid_until).toLocaleString()}</span>
								{/if}
							</td>
							<td class="px-4 py-3 text-xs text-muted-foreground">{(wh.fields ?? []).length} fields</td>
							<td class="px-4 py-3">
								{#if wh.is_active}
									<Badge class="bg-green-50 text-green-700 border-green-200">Active</Badge>
								{:else}
									<Badge variant="secondary">Inactive</Badge>
								{/if}
							</td>
							<td class="px-4 py-3 text-muted-foreground">{fmtDate(wh.created_at)}</td>
							<td class="px-4 py-3 text-right whitespace-nowrap">
								<Tooltip.Root>
									<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => toggleDeliveries(wh.id)}>
										<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><line x1="3" y1="6" x2="3.01" y2="6"/><line x1="3" y1="12" x2="3.01" y2="12"/><line x1="3" y1="18" x2="3.01" y2="18"/></svg>
									</Tooltip.Trigger>
									<Tooltip.Content>{openDeliveries === wh.id ? 'Hide deliveries' : 'Recent deliveries'}</Tooltip.Content>
								</Tooltip.Root>
								<Tooltip.Root>
									<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => { rotateId = wh.id; rotateOpen = true; }}>
										<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg>
									</Tooltip.Trigger>
									<Tooltip.Content>Rotate signing secret</Tooltip.Content>
								</Tooltip.Root>
								<Tooltip.Root>
									<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => del(wh.id)}>
										<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4h6v2"/></svg>
									</Tooltip.Trigger>
									<Tooltip.Content>Delete webhook</Tooltip.Content>
								</Tooltip.Root>
							</td>
						</tr>
						{#if openDeliveries === wh.id}
							<tr class="bg-muted/20">
								<td colspan="6" class="px-4 py-3">
									{#if deliveriesLoading}
										<p class="text-xs text-muted-foreground">Loading deliveries…</p>
									{:else if deliveries.length === 0}
										<p class="text-xs text-muted-foreground">No deliveries yet for this webhook.</p>
									{:else}
										<table class="w-full text-xs">
											<thead>
												<tr class="text-left text-muted-foreground">
													<th class="py-1 pr-4 font-medium">Event</th>
													<th class="py-1 pr-4 font-medium">Status</th>
													<th class="py-1 pr-4 font-medium">HTTP</th>
													<th class="py-1 pr-4 font-medium">Attempts</th>
													<th class="py-1 pr-4 font-medium">Last attempt</th>
													<th class="py-1"></th>
												</tr>
											</thead>
											<tbody class="divide-y divide-border/50">
												{#each deliveries as d}
													<tr>
														<td class="py-1 pr-4 font-mono">{d.event}</td>
														<td class="py-1 pr-4">
															<span class={d.status === 'success' || d.status === 'delivered' ? 'text-green-700' : d.status === 'failed' ? 'text-destructive' : 'text-muted-foreground'}>{d.status}</span>
														</td>
														<td class="py-1 pr-4">{d.response_status ?? '—'}</td>
														<td class="py-1 pr-4">{d.attempt_count}</td>
														<td class="py-1 pr-4 text-muted-foreground">{d.last_attempted_at ? new Date(d.last_attempted_at).toLocaleString() : '—'}</td>
														<td class="py-1 text-right">
															<Button variant="ghost" size="sm" class="h-7 text-xs" disabled={redelivering === d.id}
																onclick={() => redeliver(wh.id, d.id)}>
																{redelivering === d.id ? 'Queuing…' : 'Redeliver'}
															</Button>
														</td>
													</tr>
												{/each}
											</tbody>
										</table>
									{/if}
								</td>
							</tr>
						{/if}
					{/each}
				</Tooltip.Provider>
			</tbody>
		</table>
	</div>
{/if}
