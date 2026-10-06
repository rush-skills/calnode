<script lang="ts">
	// A small WYSIWYG editor for admin-authored rich text (calendar invite message,
	// email notes). TipTap (ProseMirror) does the editing; the toolbar is built from the
	// shadcn Button/Tooltip/Popover primitives so it looks like the rest of the admin.
	// Output is HTML; the server sanitizes it on save and on send, so nothing here is
	// the security boundary — the allowlist lives in internal/richtext.
	import { onMount, onDestroy } from 'svelte';
	import { Editor } from '@tiptap/core';
	import StarterKit from '@tiptap/starter-kit';
	import Link from '@tiptap/extension-link';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import * as Popover from '$lib/components/ui/popover';
	import BoldIcon from '@lucide/svelte/icons/bold';
	import ItalicIcon from '@lucide/svelte/icons/italic';
	import ListIcon from '@lucide/svelte/icons/list';
	import ListOrderedIcon from '@lucide/svelte/icons/list-ordered';
	import Heading2Icon from '@lucide/svelte/icons/heading-2';
	import LinkIcon from '@lucide/svelte/icons/link';
	import UnlinkIcon from '@lucide/svelte/icons/unlink';
	import { cn } from '$lib/utils.js';

	let {
		value = $bindable(''),
		placeholder = '',
		id = undefined,
		class: className = '',
		minHeight = 'min-h-24',
		editable = true,
	}: {
		value?: string;
		placeholder?: string;
		id?: string;
		class?: string;
		minHeight?: string;
		/** False renders the content read-only: TipTap's setEditable drops contenteditable
		 *  on the ProseMirror element and the toolbar is disabled. A wrapping
		 *  `<fieldset disabled>` does NOT reach a contenteditable div, hence this prop. */
		editable?: boolean;
	} = $props();

	let host = $state<HTMLDivElement | null>(null);
	// The Editor is deliberately NOT $state: it is a large mutable object and making it
	// reactive both proxies nothing useful and lets Svelte see its internals change
	// under an effect. Reactivity comes from `tick`, bumped on every transaction.
	let editor: Editor | null = null;
	let ready = $state(false);
	let tick = $state(0);
	// The last HTML the editor emitted or was given. The sync effect below compares
	// against this, not against editor.getHTML(): TipTap normalises "" to "<p></p>",
	// and comparing to the normalised form would re-set content forever.
	let lastHtml = '';
	let linkOpen = $state(false);
	let linkUrl = $state('');

	onMount(() => {
		editor = new Editor({
			element: host!,
			extensions: [
				StarterKit.configure({
					heading: { levels: [2, 3] },
					// Not offered in the toolbar; keeps pasted content within what the
					// server allowlist keeps anyway.
					codeBlock: false,
					code: false,
					horizontalRule: false,
					link: false,
				}),
				Link.configure({
					openOnClick: false,
					autolink: true,
					defaultProtocol: 'https',
					HTMLAttributes: { rel: 'nofollow noopener', target: '_blank' },
				}),
			],
			content: value,
			editable,
			editorProps: {
				attributes: {
					class: cn('rte-content outline-none', minHeight, 'px-3 py-2 text-sm'),
					...(id ? { id } : {}),
					'aria-multiline': 'true',
					role: 'textbox',
				},
			},
			onTransaction: () => {
				tick++;
			},
			onUpdate: ({ editor: e }) => {
				lastHtml = e.getHTML();
				value = lastHtml;
			},
		});
		lastHtml = value;
		ready = true;
	});

	onDestroy(() => {
		editor?.destroy();
	});

	// Programmatic changes to `value` (a page loading its data after mount) flow into
	// the editor; the editor's own updates are already in `value`, so they no-op.
	$effect(() => {
		const v = value;
		if (!ready || !editor || v === lastHtml) return;
		lastHtml = v;
		editor.commands.setContent(v || '', { emitUpdate: false });
	});

	// `editable` can change after mount (a page learns the viewer cannot edit once its
	// data loads). setEditable toggles contenteditable on the ProseMirror element itself.
	$effect(() => {
		const e = editable;
		if (!ready || !editor) return;
		editor.setEditable(e);
		if (!e) linkOpen = false;
	});

	const isActive = (name: string, attrs?: Record<string, unknown>) => {
		void tick;
		return (ready && editor?.isActive(name, attrs)) || false;
	};
	const isEmpty = () => {
		void tick;
		return !ready || (editor?.isEmpty ?? true);
	};

	function openLink() {
		linkUrl = editor?.getAttributes('link').href ?? '';
		linkOpen = true;
	}
	function applyLink() {
		if (!editor) return;
		const url = linkUrl.trim();
		if (url === '') {
			editor.chain().focus().unsetLink().run();
		} else {
			editor.chain().focus().extendMarkRange('link').setLink({ href: url }).run();
		}
		linkOpen = false;
	}
	function removeLink() {
		editor?.chain().focus().unsetLink().run();
	}
</script>

{#snippet tool(label: string, active: boolean, onclick: () => void, Icon: typeof BoldIcon)}
	<Tooltip.Root>
		<Tooltip.Trigger>
			{#snippet child({ props })}
				<Button
					{...props}
					type="button"
					variant={active ? 'secondary' : 'ghost'}
					size="icon"
					class="size-7"
					aria-label={label}
					aria-pressed={active}
					disabled={!editable}
					{onclick}
				>
					<Icon class="size-4" />
				</Button>
			{/snippet}
		</Tooltip.Trigger>
		<Tooltip.Content>{label}</Tooltip.Content>
	</Tooltip.Root>
{/snippet}

<div
	data-slot="rich-text-editor"
	class={cn(
		'border-input dark:bg-input/30 focus-within:border-ring focus-within:ring-ring/50 rounded-lg border bg-transparent transition-colors focus-within:ring-3',
		className
	)}
>
	<Tooltip.Provider>
		<div class="flex flex-wrap items-center gap-0.5 border-b px-1.5 py-1" role="toolbar" aria-label="Formatting">
			{@render tool('Bold', isActive('bold'), () => editor?.chain().focus().toggleBold().run(), BoldIcon)}
			{@render tool('Italic', isActive('italic'), () => editor?.chain().focus().toggleItalic().run(), ItalicIcon)}
			{@render tool('Heading', isActive('heading', { level: 2 }), () => editor?.chain().focus().toggleHeading({ level: 2 }).run(), Heading2Icon)}
			{@render tool('Bullet list', isActive('bulletList'), () => editor?.chain().focus().toggleBulletList().run(), ListIcon)}
			{@render tool('Numbered list', isActive('orderedList'), () => editor?.chain().focus().toggleOrderedList().run(), ListOrderedIcon)}
			<Popover.Root bind:open={linkOpen}>
				<Popover.Trigger>
					{#snippet child({ props })}
						<Button
							{...props}
							type="button"
							variant={isActive('link') ? 'secondary' : 'ghost'}
							size="icon"
							class="size-7"
							aria-label="Link"
							aria-pressed={isActive('link')}
							disabled={!editable}
							onclick={openLink}
						>
							<LinkIcon class="size-4" />
						</Button>
					{/snippet}
				</Popover.Trigger>
				<Popover.Content class="w-80 space-y-2" align="start">
					<p class="text-xs font-medium">Link URL</p>
					<Input
						bind:value={linkUrl}
						placeholder="https://…"
						aria-label="Link URL"
						onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); applyLink(); } }}
					/>
					<div class="flex justify-end gap-2">
						<Button type="button" variant="ghost" size="sm" onclick={() => (linkOpen = false)}>Cancel</Button>
						<Button type="button" size="sm" onclick={applyLink}>Apply</Button>
					</div>
				</Popover.Content>
			</Popover.Root>
			{#if isActive('link')}
				{@render tool('Remove link', false, removeLink, UnlinkIcon)}
			{/if}
		</div>
	</Tooltip.Provider>
	<div class="relative">
		{#if isEmpty() && placeholder}
			<p class="pointer-events-none absolute left-3 top-2 text-sm text-muted-foreground" aria-hidden="true">{placeholder}</p>
		{/if}
		<div bind:this={host}></div>
	</div>
</div>

<style>
	/* ProseMirror's content element carries these classes (see editorProps above).
	   Block typography is scoped here so the editor shows what the invite will show. */
	:global(.rte-content h2) { font-size: 1.125rem; font-weight: 600; margin: 0.5rem 0 0.25rem; }
	:global(.rte-content h3) { font-size: 1rem; font-weight: 600; margin: 0.5rem 0 0.25rem; }
	:global(.rte-content p) { margin: 0.25rem 0; }
	:global(.rte-content ul) { list-style: disc; padding-left: 1.25rem; margin: 0.25rem 0; }
	:global(.rte-content ol) { list-style: decimal; padding-left: 1.25rem; margin: 0.25rem 0; }
	:global(.rte-content a) { text-decoration: underline; text-underline-offset: 2px; }
	:global(.rte-content blockquote) { border-left: 2px solid var(--border); padding-left: 0.75rem; color: var(--muted-foreground); }
</style>
