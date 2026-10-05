/**
 * Client-side mirror of slugify() in internal/handler/teams.go, for the live
 * "/book/…" preview in the event-type forms. Display only: the server normalises
 * every slug it stores and is the authority — always use the slug it returns.
 *
 * Rules (keep in step with Go): trim, lowercase, keep ASCII letters and digits, turn
 * runs of space/hyphen/underscore into a single hyphen, drop everything else, and
 * strip leading/trailing hyphens.
 */
export function slugify(s: string): string {
	let out = '';
	let lastHyphen = false;
	for (const ch of s.trim().toLowerCase()) {
		if ((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
			out += ch;
			lastHyphen = false;
		} else if (ch === ' ' || ch === '-' || ch === '_') {
			if (out.length > 0 && !lastHyphen) {
				out += '-';
				lastHyphen = true;
			}
		}
	}
	return out.replace(/^-+|-+$/g, '');
}
