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
	import { t, tr } from '$lib/i18n';

	const loadingFlag = createAsyncFlag(true);
	const savingFlag = createAsyncFlag();

	// One address per line in the textarea; commas are accepted too so a pasted list works.
	let emailsText = $state('');
	let calendarMessage = $state('');

	// The page only renders its form for admins; the editor still gets told explicitly,
	// because a disabled fieldset does not reach a contenteditable element.
	const canEdit = $derived(!!$currentUser?.is_admin);

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
		}, tr('settings.participants.toast.loadError'))
	);

	async function save() {
		await savingFlag.run(async () => {
			const s = await api.patch<ParticipantSettings>('/v1/settings/participants', {
				default_attendee_emails: parseEmails(emailsText),
				default_calendar_message: normalizeHtml(calendarMessage)
			});
			emailsText = (s.default_attendee_emails ?? []).join('\n');
			calendarMessage = s.default_calendar_message ?? '';
			toast.success(tr('settings.participants.toast.saved'));
		}, tr('settings.participants.toast.saveError'));
	}
</script>

<svelte:window onkeydown={saveOnCmdS(save, () => !savingFlag.active)} />

{#if !$currentUser?.is_admin}
	<p class="text-sm text-muted-foreground">{$t('common.adminRequired')}</p>
{:else if loadingFlag.active}
	<p class="py-8 text-sm text-muted-foreground">{$t('common.loading')}</p>
{:else}
	<div class="max-w-2xl space-y-6">
		<div class="rounded-lg border bg-card p-6">
			<h2 class="text-sm font-semibold">{$t('settings.participants.defaultParticipants.title')}</h2>
			<p class="mt-0.5 text-xs text-muted-foreground">
				{$t('settings.participants.defaultParticipants.description')}
			</p>
			<div class="mt-4 space-y-1.5">
				<Label for="default-participants">{$t('settings.participants.defaultParticipants.emailsLabel')}</Label>
				<Textarea
					id="default-participants"
					bind:value={emailsText}
					rows={5}
					class="font-mono text-xs"
					placeholder={'notes@example.com\nteam@example.com'}
				/>
				<p class="text-xs text-muted-foreground">
					{$t('settings.participants.defaultParticipants.emailsHint')}
				</p>
			</div>
		</div>

		<div class="rounded-lg border bg-card p-6">
			<h2 class="text-sm font-semibold">{$t('settings.participants.inviteMessage.title')}</h2>
			<p class="mt-0.5 text-xs text-muted-foreground">
				{$t('settings.participants.inviteMessage.description')}
			</p>
			<div class="mt-4 space-y-1.5">
				<Label for="default-calendar-message">{$t('settings.participants.inviteMessage.messageLabel')}</Label>
				<RichTextEditor id="default-calendar-message" bind:value={calendarMessage} editable={canEdit} placeholder={$t('settings.participants.inviteMessage.placeholder')} />
			</div>
		</div>

		<Button onclick={save} disabled={savingFlag.active}>{savingFlag.active ? $t('common.saving') : $t('common.save')}</Button>
	</div>
{/if}
