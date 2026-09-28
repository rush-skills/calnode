<script lang="ts">
	// Test harness: binds the editor's value and exposes it, plus a way to set it
	// from outside so the programmatic-update path is exercised too.
	import RichTextEditor from './RichTextEditor.svelte';
	let { initial = '' }: { initial?: string } = $props();
	let value = $state(initial);
</script>

<RichTextEditor bind:value placeholder="Type here…" id="rte" />
<pre data-testid="out">{value}</pre>
<button type="button" data-testid="set-external" onclick={() => (value = '<p>from outside</p>')}>set</button>
<button type="button" data-testid="clear-external" onclick={() => (value = '')}>clear</button>
