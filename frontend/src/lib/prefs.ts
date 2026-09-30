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

function allTimezones(): string[] {
	try {
		const intl = Intl as unknown as { supportedValuesOf?: (k: string) => string[] };
		const list = intl.supportedValuesOf?.('timeZone');
		if (list && list.length) return list.includes('UTC') ? list : [...list, 'UTC'];
	} catch {
		// fall through
	}
	return FALLBACK_TIMEZONES;
}

export const TIMEZONES = allTimezones();

/** The picker's options: every zone the browser knows, plus `current` if it is not among
 *  them (a zone stored via the API or an older alias), so the stored value is never
 *  silently blanked out of the form. */
export function timezoneOptions(current: string): string[] {
	if (current && !TIMEZONES.includes(current)) return [current, ...TIMEZONES];
	return TIMEZONES;
}
