import { describe, test, expect, afterEach } from 'vitest';
import { render, cleanup } from 'vitest-browser-svelte';
import { userEvent } from '@vitest/browser/context';
import Harness from './Harness.test.svelte';

afterEach(cleanup);

const out = (c: Element) => c.querySelector('[data-testid="out"]')!.textContent ?? '';
const editorOf = (c: Element) => c.querySelector('#rte') as HTMLElement;
const tool = (c: Element, label: string) => c.querySelector(`button[aria-label="${label}"]`) as HTMLButtonElement;
// Popover content is portalled to <body>, so buttons inside it are found by their text.
const byText = (text: string) =>
	[...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === text) as HTMLElement;

// Wait until the bound value satisfies `pred`; ProseMirror updates asynchronously.
async function until(c: Element, pred: (v: string) => boolean, ms = 3000) {
	const start = Date.now();
	while (Date.now() - start < ms) {
		if (pred(out(c))) return;
		await new Promise((r) => setTimeout(r, 25));
	}
	throw new Error(`timed out; value = ${out(c)}`);
}

describe('RichTextEditor', () => {
	test('renders the initial HTML and shows no placeholder', async () => {
		const { container } = await render(Harness, { props: { initial: '<p>Hello <strong>world</strong></p>' } });
		const ed = editorOf(container);
		expect(ed.getAttribute('contenteditable')).toBe('true');
		expect(ed.innerHTML).toContain('<strong>world</strong>');
		expect(container.textContent).not.toContain('Type here…');
	});

	test('placeholder shows when empty and hides once there is text', async () => {
		const { container } = await render(Harness, { props: { initial: '' } });
		expect(container.textContent).toContain('Type here…');
		await userEvent.click(editorOf(container));
		await userEvent.keyboard('Hi');
		await until(container, (v) => v.includes('Hi'));
		expect(container.textContent).not.toContain('Type here…');
		expect(out(container)).toBe('<p>Hi</p>');
	});

	test('toolbar toggles bold, italic, heading and lists into the bound HTML', async () => {
		const { container } = await render(Harness, { props: { initial: '' } });
		await userEvent.click(editorOf(container));

		await userEvent.click(tool(container, 'Bold'));
		expect(tool(container, 'Bold').getAttribute('aria-pressed')).toBe('true');
		await userEvent.keyboard('bold');
		await until(container, (v) => v.includes('<strong>bold</strong>'));
		await userEvent.click(tool(container, 'Bold'));
		expect(tool(container, 'Bold').getAttribute('aria-pressed')).toBe('false');

		await userEvent.click(tool(container, 'Italic'));
		await userEvent.keyboard('it');
		await until(container, (v) => v.includes('<em>it</em>'));

		await userEvent.click(tool(container, 'Heading'));
		await until(container, (v) => v.startsWith('<h2>'));
		expect(tool(container, 'Heading').getAttribute('aria-pressed')).toBe('true');
		await userEvent.click(tool(container, 'Heading'));
		await until(container, (v) => v.startsWith('<p>'));

		await userEvent.click(tool(container, 'Bullet list'));
		await until(container, (v) => v.startsWith('<ul><li>'));
		await userEvent.click(tool(container, 'Numbered list'));
		await until(container, (v) => v.startsWith('<ol><li>'));
		await userEvent.click(tool(container, 'Numbered list'));
		await until(container, (v) => v.startsWith('<p>'));
	});

	test('link popover sets, then removes, a link on the selection', async () => {
		const { container } = await render(Harness, { props: { initial: '<p>read this</p>' } });
		const ed = editorOf(container);
		await userEvent.click(ed);
		await userEvent.keyboard('{Control>}a{/Control}');

		await userEvent.click(tool(container, 'Link'));
		const input = document.querySelector('input[aria-label="Link URL"]') as HTMLInputElement;
		expect(input).not.toBeNull();
		await userEvent.fill(input, 'https://example.com/x');
		await userEvent.keyboard('{Enter}');
		await until(container, (v) => v.includes('href="https://example.com/x"'));
		expect(out(container)).toContain('rel="nofollow noopener"');
		expect(out(container)).toContain('target="_blank"');
		expect(tool(container, 'Link').getAttribute('aria-pressed')).toBe('true');

		// Reopening shows the current href; an empty URL applied removes the link.
		await userEvent.click(tool(container, 'Link'));
		const again = document.querySelector('input[aria-label="Link URL"]') as HTMLInputElement;
		expect(again.value).toBe('https://example.com/x');
		await userEvent.click(byText('Cancel'));
		expect(out(container)).toContain('href=');

		await userEvent.click(tool(container, 'Remove link'));
		await until(container, (v) => !v.includes('href='));
		expect(tool(container, 'Remove link')).toBeNull();
	});

	test('link popover: applying an empty URL unsets the link', async () => {
		const { container } = await render(Harness, { props: { initial: '<p><a href="https://a.b/">linked</a></p>' } });
		await userEvent.click(editorOf(container));
		await userEvent.keyboard('{Control>}a{/Control}');
		await userEvent.click(tool(container, 'Link'));
		const input = document.querySelector('input[aria-label="Link URL"]') as HTMLInputElement;
		await userEvent.fill(input, '   ');
		await userEvent.click(byText('Apply'));
		await until(container, (v) => !v.includes('href='));
	});

	test('a value set from outside replaces the editor content; clearing empties it', async () => {
		const { container } = await render(Harness, { props: { initial: '<p>start</p>' } });
		await userEvent.click(container.querySelector('[data-testid="set-external"]') as HTMLElement);
		await until(container, (v) => v === '<p>from outside</p>');
		expect(editorOf(container).textContent).toBe('from outside');
		await userEvent.click(container.querySelector('[data-testid="clear-external"]') as HTMLElement);
		await until(container, (v) => v === '');
		expect(container.textContent).toContain('Type here…');
	});
});
