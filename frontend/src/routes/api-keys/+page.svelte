<script lang="ts">
	import { onMount } from 'svelte';
	import { api, type APIKey } from '$lib/api';
	import { Button, buttonVariants } from '$lib/components/ui/button';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Checkbox } from '$lib/components/ui/checkbox';
	import { Badge } from '$lib/components/ui/badge';
	import * as Tooltip from '$lib/components/ui/tooltip';

	let items: APIKey[] = $state([]);
	let loading = $state(true);
	let error = $state('');
	let showCreate = $state(false);
	let newName = $state('');
	let creating = $state(false);
	let createError = $state('');
	let newKey = $state('');
	// Read-only scopes. None ticked = a full key that acts with your role.
	const SCOPES = [
		{ key: 'bookings:read', label: 'Read bookings', help: 'GET /v1/bookings and /v1/bookings/{id}' },
		{ key: 'webhooks:read', label: 'Read webhooks', help: 'GET /v1/webhooks and their delivery log' },
	];
	let newScopes = $state<string[]>([]);
	let revokeOpen = $state(false);
	let revokeTarget = $state<{ id: string; name: string } | null>(null);

	async function load() {
		try {
			const res = await api.get<{ items: APIKey[] }>('/v1/api-keys');
			items = res.items;
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}

	onMount(load);

	async function create() {
		createError = '';
		if (!newName.trim()) { createError = 'Name is required.'; return; }
		creating = true;
		try {
			const body: { name: string; scopes?: string[] } = { name: newName.trim() };
			if (newScopes.length > 0) body.scopes = newScopes;
			const res = await api.post<{ key: string }>('/v1/api-keys', body);
			newKey = res.key;
			newName = '';
			newScopes = [];
			showCreate = false;
			await load();
		} catch (e: any) {
			createError = e.message;
		} finally {
			creating = false;
		}
	}

	function revoke(id: string, name: string) {
		revokeTarget = { id, name };
		revokeOpen = true;
	}

	async function doRevoke() {
		if (!revokeTarget) return;
		try {
			await api.del(`/v1/api-keys/${revokeTarget.id}`);
			await load();
		} catch (e: any) {
			error = e.message;
		}
	}

	function fmtDate(iso: string) {
		return new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' });
	}

	function copyKey() {
		navigator.clipboard.writeText(newKey).catch(() => {});
	}
</script>

<ConfirmDialog
	bind:open={revokeOpen}
	title="Revoke API key?"
	description={revokeTarget ? `Revoke "${revokeTarget.name}"? Any integrations using it will stop working immediately.` : ''}
	confirmText="Revoke"
	destructive
	onConfirm={doRevoke}
/>

<svelte:head><title>API Keys — Calnode</title></svelte:head>

<div class="mb-8 flex items-center justify-between">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">API Keys</h1>
		<p class="mt-1 text-sm text-muted-foreground">Authenticate CLI tools and integrations.</p>
	</div>
	<Button onclick={() => { showCreate = !showCreate; createError = ''; newKey = ''; }}>
		{showCreate ? 'Cancel' : 'New key'}
	</Button>
</div>

{#if newKey}
	<div class="mb-6 rounded-lg border border-green-200 bg-green-50 p-4">
		<p class="mb-2 text-sm font-medium text-green-800">Key created — copy it now. It won't be shown again.</p>
		<div class="mb-3 rounded-md border bg-white px-3 py-2 font-mono text-xs text-foreground break-all">{newKey}</div>
		<Button variant="outline" size="sm" onclick={copyKey}>
			Copy to clipboard
		</Button>
	</div>
{/if}

{#if showCreate}
	<div class="mb-6 rounded-lg border bg-card p-6">
		<h2 class="mb-4 text-sm font-semibold">New API key</h2>
		{#if createError}<p class="mb-3 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{createError}</p>{/if}
		<div class="mb-4 max-w-sm space-y-1.5">
			<Label for="key-name">Key name</Label>
			<Input
				id="key-name"
				bind:value={newName}
				placeholder="e.g. CI/CD pipeline"
			/>
		</div>
		<div class="mb-4 space-y-1.5">
			<p class="text-sm font-medium">Limit to read-only access <span class="font-normal text-muted-foreground">(optional)</span></p>
			{#each SCOPES as sc}
				<label class="flex cursor-pointer items-start gap-2 text-sm">
					<Checkbox class="mt-0.5" checked={newScopes.includes(sc.key)}
						onCheckedChange={(v) => (newScopes = v === true ? [...newScopes, sc.key] : newScopes.filter((x) => x !== sc.key))} />
					<span>{sc.label} <span class="font-mono text-xs text-muted-foreground">{sc.key}</span>
						<span class="block text-xs text-muted-foreground">{sc.help}</span></span>
				</label>
			{/each}
			<p class="text-xs text-muted-foreground">
				{newScopes.length > 0
					? 'This key can only make those reads, and nothing else, whatever your role. Made by an admin, it reads every booking in the workspace.'
					: 'With nothing ticked the key can do everything you can.'}
			</p>
		</div>
		<Button onclick={create} disabled={creating}>
			{creating ? 'Creating…' : 'Create key'}
		</Button>
	</div>
{/if}

{#if error}<p class="mb-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</p>{/if}

{#if loading}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{:else if items.length === 0}
	<div class="rounded-lg border border-dashed bg-card p-12 text-center">
		<p class="text-sm font-medium">No API keys</p>
		<p class="mt-1 text-sm text-muted-foreground">Create a key to authenticate CLI tools and integrations.</p>
	</div>
{:else}
	<div class="rounded-lg border bg-card overflow-hidden">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b">
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">Name</th>
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">Created</th>
					<th class="px-4 pb-3 pt-3 text-left text-xs font-medium text-muted-foreground">Last used</th>
					<th class="px-4 pb-3 pt-3"></th>
				</tr>
			</thead>
			<tbody class="divide-y">
				<Tooltip.Provider>
					{#each items as k}
						<tr class="transition-colors hover:bg-muted/30">
							<td class="px-4 py-3 font-medium">
								{k.name}
								{#if k.scopes && k.scopes.length > 0}
									{#each k.scopes as sc}<Badge variant="secondary" class="ml-1.5 font-mono text-[10px]">{sc}</Badge>{/each}
								{:else}
									<Badge variant="outline" class="ml-1.5 text-[10px]">Full access</Badge>
								{/if}
							</td>
							<td class="px-4 py-3 text-muted-foreground">{fmtDate(k.created_at)}</td>
							<td class="px-4 py-3 text-muted-foreground">
								{#if k.last_used_at}{fmtDate(k.last_used_at)}{:else}Never{/if}
							</td>
							<td class="px-4 py-3 text-right">
								<Tooltip.Root>
									<Tooltip.Trigger class={buttonVariants({ variant: 'ghost', size: 'icon' })} onclick={() => revoke(k.id, k.name)}>
										<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/><path d="M9 6V4h6v2"/></svg>
									</Tooltip.Trigger>
									<Tooltip.Content>Revoke key</Tooltip.Content>
								</Tooltip.Root>
							</td>
						</tr>
					{/each}
				</Tooltip.Provider>
			</tbody>
		</table>
	</div>
{/if}
