<script lang="ts">
	import { onMount } from 'svelte';
	import { api, type ParticipantSettings } from '$lib/api';
	import { currentUser } from '$lib/stores';
	import { Button } from '$lib/components/ui/button';
	import { Label } from '$lib/components/ui/label';
	import { Textarea } from '$lib/components/ui/textarea';
	import { RichTextEditor, normalizeHtml } from '$lib/components/rich-text-editor';
	import { toast } from 'svelte-sonner';
	import { saveOnCmdS } from '$lib/save-shortcut';
	import { createAsyncFlag } from '$lib/async-action.svelte';

	const loadingFlag = createAsyncFlag(true);
	const savingFlag = createAsyncFlag();

	// One address per line in the textarea; commas are accepted too so a pasted list works.
	let emailsText = $state('');
	let calendarMessage = $state('');

	function parseEmails(text: string): string[] {
		return text
			.split(/[\n,]/)
			.map((s) => s.trim())
			.filter((s) => s.length > 0);
	}

	onMount(() =>
		loadingFlag.run(async () => {
			const s = await api.get<ParticipantSettings>('/v1/settings/participants');
			emailsText = (s.default_attendee_emails ?? []).join('\n');
			calendarMessage = s.default_calendar_message ?? '';
		}, 'Could not load meeting defaults')
	);

	async function save() {
		await savingFlag.run(async () => {
			const s = await api.patch<ParticipantSettings>('/v1/settings/participants', {
				default_attendee_emails: parseEmails(emailsText),
				default_calendar_message: normalizeHtml(calendarMessage)
			});
			emailsText = (s.default_attendee_emails ?? []).join('\n');
			calendarMessage = s.default_calendar_message ?? '';
			toast.success('Meeting defaults saved');
		}, 'Could not save meeting defaults');
	}
</script>

<svelte:window onkeydown={saveOnCmdS(save, () => !savingFlag.active)} />

{#if !$currentUser?.is_admin}
	<p class="text-sm text-muted-foreground">Admin access required.</p>
{:else if loadingFlag.active}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{:else}
	<div class="max-w-2xl space-y-6">
		<div class="rounded-lg border bg-card p-6">
			<h2 class="text-sm font-semibold">Default participants</h2>
			<p class="mt-0.5 text-xs text-muted-foreground">
				These addresses are invited to the calendar event of every new meeting booked here, for every event type, including
				live events. They get the calendar invite only — Calnode does not email them and bookers
				never see them. Typical use: a notetaker bot that joins when a shared mailbox is invited.
			</p>
			<div class="mt-4 space-y-1.5">
				<Label for="default-participants">Email addresses</Label>
				<Textarea
					id="default-participants"
					bind:value={emailsText}
					rows={5}
					class="font-mono text-xs"
					placeholder={'notes@example.com\nteam@example.com'}
				/>
				<p class="text-xs text-muted-foreground">
					One address per line (commas work too). Up to 20. The invite comes from each host's own
					connected calendar, so a host without a connected calendar sends none. Google and
					Microsoft email the invite themselves; on CalDAV the addresses are written to the event
					and whether an invite goes out depends on the server's scheduling support.
				</p>
			</div>
		</div>

		<div class="rounded-lg border bg-card p-6">
			<h2 class="text-sm font-semibold">Default invite message</h2>
			<p class="mt-0.5 text-xs text-muted-foreground">
				Added to the calendar invite of every new booking, for every event type, after the event
				type's own invite message. Also added to live events. Use it for notices like "This call
				may be recorded for quality and training purposes." Changes apply to bookings made from
				now on; invites already sent are not changed.
			</p>
			<div class="mt-4 space-y-1.5">
				<Label for="default-calendar-message">Message</Label>
				<RichTextEditor id="default-calendar-message" bind:value={calendarMessage} placeholder="This call may be recorded for quality and training purposes." />
			</div>
		</div>

		<Button onclick={save} disabled={savingFlag.active}>{savingFlag.active ? 'Saving…' : 'Save'}</Button>
	</div>
{/if}
