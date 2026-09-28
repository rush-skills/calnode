import { describe, test, expect } from 'vitest';
import { isEmptyHtml, normalizeHtml } from './html';

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
