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
