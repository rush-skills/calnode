import { writable, get } from 'svelte/store';
import type { User } from './api';

export interface UserPrefs {
	timezone: string;
	time_format: '12h' | '24h';
	week_start: number;
	date_format: 'dmy' | 'mdy' | 'ymd';
}

const defaults: UserPrefs = {
	timezone: 'UTC',
	time_format: '12h',
	week_start: 1,
	date_format: 'dmy'
};

export const prefs = writable<UserPrefs>(defaults);

export function prefsFromUser(u: User): UserPrefs {
	return {
		timezone: u.timezone,
		time_format: u.time_format ?? '12h',
		week_start: u.week_start ?? 1,
		date_format: u.date_format ?? 'dmy'
	};
}

function fmtDatePart(date: Date, format: 'dmy' | 'mdy' | 'ymd'): string {
	const d = String(date.getDate()).padStart(2, '0');
	const m = String(date.getMonth() + 1).padStart(2, '0');
	const y = date.getFullYear();
	if (format === 'mdy') return `${m}/${d}/${y}`;
	if (format === 'ymd') return `${y}-${m}-${d}`;
	return `${d}/${m}/${y}`;
}

export function fmtDateTime(iso: string, p: UserPrefs = get(prefs)): string {
	const date = new Date(iso);
	const datePart = fmtDatePart(date, p.date_format);
	const timePart = date.toLocaleTimeString(undefined, {
		hour: '2-digit',
		minute: '2-digit',
		hour12: p.time_format === '12h'
	});
	return `${datePart}, ${timePart}`;
}

export function fmtDate(ymd: string, p: UserPrefs = get(prefs)): string {
	// ymd is always YYYY-MM-DD from the API — parse directly to avoid TZ shift from new Date()
	const [y, m, d] = ymd.split('-');
	if (p.date_format === 'mdy') return `${m}/${d}/${y}`;
	if (p.date_format === 'ymd') return `${y}-${m}-${d}`;
	return `${d}/${m}/${y}`;
}

export function fmtTime(iso: string, p: UserPrefs = get(prefs)): string {
	return new Date(iso).toLocaleTimeString(undefined, {
		hour: '2-digit',
		minute: '2-digit',
		hour12: p.time_format === '12h'
	});
}

export const WEEK_DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

// Fallback only, for browsers without Intl.supportedValuesOf (pre-2022). Modern browsers
// return the full IANA table (~420 zones), which is what the picker shows.
const FALLBACK_TIMEZONES = [
	'Pacific/Auckland',
	'Australia/Sydney',
	'Australia/Melbourne',
	'Asia/Tokyo',
	'Asia/Singapore',
	'Asia/Kolkata',
	'Asia/Dubai',
	'Europe/London',
	'Europe/Paris',
	'Europe/Berlin',
	'Europe/Amsterdam',
	'America/New_York',
	'America/Chicago',
	'America/Denver',
	'America/Los_Angeles',
	'UTC'
];

// Mirrors TZ_RENAMED in internal/handler/assets/booking-logic.js (and its copy in embed.js):
// IANA zones that were renamed. Chromium's Intl list still reports several old names
// (Asia/Calcutta, Europe/Kiev, Asia/Saigon…), so the picker canonicalises them, stores the
// current name, and keeps the old one as a search alias. Keep the three tables in step.
export const TZ_RENAMED: Record<string, string> = {
	'Asia/Calcutta': 'Asia/Kolkata', 'Asia/Saigon': 'Asia/Ho_Chi_Minh', 'Asia/Katmandu': 'Asia/Kathmandu',
	'Asia/Rangoon': 'Asia/Yangon', 'Asia/Ulan_Bator': 'Asia/Ulaanbaatar', 'Asia/Dacca': 'Asia/Dhaka',
	'Asia/Thimbu': 'Asia/Thimphu', 'Asia/Ujung_Pandang': 'Asia/Makassar', 'Asia/Macao': 'Asia/Macau',
	'Europe/Kiev': 'Europe/Kyiv', 'Europe/Uzhgorod': 'Europe/Kyiv', 'Europe/Zaporozhye': 'Europe/Kyiv',
	'America/Godthab': 'America/Nuuk', 'Atlantic/Faeroe': 'Atlantic/Faroe',
	'Pacific/Truk': 'Pacific/Chuuk', 'Pacific/Ponape': 'Pacific/Pohnpei', 'Pacific/Enderbury': 'Pacific/Kanton',
	'America/Buenos_Aires': 'America/Argentina/Buenos_Aires', 'America/Catamarca': 'America/Argentina/Catamarca',
	'America/Cordoba': 'America/Argentina/Cordoba', 'America/Jujuy': 'America/Argentina/Jujuy',
	'America/Mendoza': 'America/Argentina/Mendoza', 'America/Indianapolis': 'America/Indiana/Indianapolis',
	'America/Louisville': 'America/Kentucky/Louisville', 'America/Coral_Harbour': 'America/Atikokan'
};

function validTz(zone: string): boolean {
	if (!zone) return false;
	try {
		new Intl.DateTimeFormat('en-US', { timeZone: zone });
		return true;
	} catch {
		return false;
	}
}

/** The current IANA name for `zone` (old alias → renamed zone, when the browser knows it). */
export function canonicalTz(zone: string): string {
	const renamed = TZ_RENAMED[zone];
	return renamed && validTz(renamed) ? renamed : zone;
}

/** Old names that canonicalise to `zone`, so a search for "calcutta" still finds Asia/Kolkata. */
function tzAliases(zone: string): string[] {
	return Object.keys(TZ_RENAMED).filter((old) => TZ_RENAMED[old] === zone);
}

function allTimezones(): string[] {
	let list: string[] = FALLBACK_TIMEZONES;
	try {
		const intl = Intl as unknown as { supportedValuesOf?: (k: string) => string[] };
		const got = intl.supportedValuesOf?.('timeZone');
		if (got && got.length) list = got;
	} catch {
		// fall through
	}
	// Canonicalise and de-duplicate: Chromium lists both Asia/Calcutta and Asia/Kolkata.
	const seen = new Set<string>();
	const out: string[] = [];
	for (const z of [...list, 'UTC']) {
		const id = canonicalTz(z);
		if (seen.has(id)) continue;
		seen.add(id);
		out.push(id);
	}
	return out;
}

export const TIMEZONES = allTimezones();

export interface TimezoneOption {
	value: string;
	label: string;
	/** Extra searchable text: old aliases of the zone (e.g. "Asia/Calcutta" for Asia/Kolkata). */
	keywords: string;
}

/** The picker's options: every zone the browser knows (canonical names, de-duplicated),
 *  plus `current` if it is not among them (a zone stored via the API, or a legacy alias
 *  this browser cannot canonicalise), so the stored value is never silently blanked out
 *  of the form. A stored legacy name that does canonicalise is shown under its current
 *  name, and saving stores the canonical one. */
export function timezoneOptions(current: string): TimezoneOption[] {
	const list = current && !TIMEZONES.includes(canonicalTz(current)) ? [current, ...TIMEZONES] : TIMEZONES;
	return list.map((tz) => {
		const aliases = tzAliases(tz).map((a) => `${a} ${a.replace(/[/_]/g, ' ')}`);
		return { value: tz, label: tz, keywords: [tz.replace(/[/_]/g, ' '), ...aliases].join(' ') };
	});
}
