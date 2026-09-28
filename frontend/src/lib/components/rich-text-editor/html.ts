// Helpers shared by the rich-text editor and the pages that save its output.

/**
 * True when an editor's HTML holds no visible content — TipTap reports an empty
 * document as `<p></p>`, and a document the user cleared may still carry empty
 * paragraphs or whitespace. Pages send "" for these so the API stores NULL rather
 * than a shell of tags. A deliberate line break (`<br>`) is content.
 */
export function isEmptyHtml(html: string | null | undefined): boolean {
	if (!html) return true;
	if (/<br\s*\/?>/i.test(html)) return false;
	const text = html
		.replace(/<[^>]*>/g, '')
		.replace(/&nbsp;/gi, ' ')
		.trim();
	return text === '';
}

/** The value to persist for an editor: "" when it is visually empty, else the HTML. */
export function normalizeHtml(html: string | null | undefined): string {
	return isEmptyHtml(html) ? '' : (html as string);
}

/**
 * A readable plain-text rendering of editor HTML, for previews that show the
 * text/plain email: block elements become line breaks, list items get "- ", the
 * rest of the tags are stripped and entities decoded. The server does the real
 * conversion on send (internal/richtext); this only has to look right on screen.
 */
export function htmlToText(html: string | null | undefined): string {
	if (!html) return '';
	const doc = new DOMParser().parseFromString(html, 'text/html');
	const out: string[] = [];
	const isBlock = (tag: string) => /^(p|h[1-6]|blockquote|ul|ol|div)$/.test(tag);
	const walk = (node: Node): void => {
		if (node.nodeType === Node.TEXT_NODE) {
			out.push(node.textContent ?? '');
			return;
		}
		if (node.nodeType !== Node.ELEMENT_NODE) return;
		const el = node as Element;
		const tag = el.tagName.toLowerCase();
		if (tag === 'br') {
			out.push('\n');
			return;
		}
		if (tag === 'li') out.push('\n- ');
		else if (isBlock(tag)) out.push('\n');
		el.childNodes.forEach(walk);
		if (tag === 'a') {
			const href = el.getAttribute('href') ?? '';
			const text = (el.textContent ?? '').trim();
			if (href && text && text !== href) out.push(` (${href})`);
		}
		if (isBlock(tag)) out.push('\n');
	};
	doc.body.childNodes.forEach(walk);
	return out
		.join('')
		.replace(/[ \t]+\n/g, '\n')
		.replace(/\n{3,}/g, '\n\n')
		.trim();
}
