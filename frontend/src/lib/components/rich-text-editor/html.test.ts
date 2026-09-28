import { describe, test, expect } from 'vitest';
import { isEmptyHtml, normalizeHtml, htmlToText } from './html';

describe('isEmptyHtml', () => {
	test('empty editor shells are empty', () => {
		for (const v of ['', null, undefined, '<p></p>', '<p> </p>', '<p>&nbsp;</p>', '<ul><li></li></ul>', '  ']) {
			expect(isEmptyHtml(v)).toBe(true);
		}
	});
	test('text and deliberate line breaks are content', () => {
		expect(isEmptyHtml('<p>hi</p>')).toBe(false);
		expect(isEmptyHtml('<p><br></p>')).toBe(false);
		expect(isEmptyHtml('<p><br/></p>')).toBe(false);
	});
});

describe('normalizeHtml', () => {
	test('empty becomes "" and content passes through', () => {
		expect(normalizeHtml('<p></p>')).toBe('');
		expect(normalizeHtml(undefined)).toBe('');
		expect(normalizeHtml('<p><b>x</b></p>')).toBe('<p><b>x</b></p>');
	});
});

describe('htmlToText', () => {
	test('blocks, lists, links and entities read as text', () => {
		const html = '<h2>Agenda</h2><p>First &amp; second.<br>Next line.</p><ul><li>One</li><li>Two</li></ul><p>See <a href="https://x.io/g">the guide</a> or <a href="https://x.io/y">https://x.io/y</a>.</p>';
		expect(htmlToText(html)).toBe(
			'Agenda\n\nFirst & second.\nNext line.\n\n- One\n- Two\n\nSee the guide (https://x.io/g) or https://x.io/y.'
		);
	});
	test('empty and plain input', () => {
		expect(htmlToText('')).toBe('');
		expect(htmlToText(undefined)).toBe('');
		expect(htmlToText('just text')).toBe('just text');
	});
});
