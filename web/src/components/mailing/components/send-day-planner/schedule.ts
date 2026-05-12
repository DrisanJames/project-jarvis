// schedule — pure helpers that mirror deploy_may12_mature.schedule_for().
//
// Brand stagger: DB+0 / QF+15 / HT+30 / MH+45 minutes (via index in BRANDS).
// Slot anchors: see SLOT_ANCHOR_HOUR_UTC + SLOT_ANCHOR_MINUTE_UTC.
// Window hours: per SLOT_WINDOW_HOURS.

import {
  BRANDS,
  SLOT_ANCHOR_HOUR_UTC,
  SLOT_ANCHOR_MINUTE_UTC,
  SLOT_WINDOW_HOURS,
  STAGGER_MINUTES,
} from './constants';
import type { Brand, Slot } from './types';

/** scheduleFor — given a send-date (YYYY-MM-DD), slot, and brand, return
 *  the (startUTC, endUTC) ISO strings. Brand index in BRANDS supplies
 *  the stagger offset. */
export function scheduleFor(
  sendDate: string, slot: Slot, brand: Brand,
): { startAtUTC: string; endAtUTC: string; windowHours: number } {
  // sendDate is YYYY-MM-DD in MDT — but the anchors are UTC. The Python
  // builds a UTC datetime directly from the date components, so we do
  // the same: SEND_DATE.year/month/day with anchor hour/minute UTC.
  const [yyyy, mm, dd] = sendDate.split('-').map(s => parseInt(s, 10));
  const baseHour = SLOT_ANCHOR_HOUR_UTC[slot];
  const baseMinute = SLOT_ANCHOR_MINUTE_UTC;
  const idx = BRANDS.indexOf(brand);
  const startMs = Date.UTC(yyyy, (mm - 1), dd, baseHour, baseMinute, 0)
    + idx * STAGGER_MINUTES * 60 * 1000;
  const window = SLOT_WINDOW_HOURS[slot];
  const endMs = startMs + window * 3600 * 1000;
  return {
    startAtUTC: new Date(startMs).toISOString(),
    endAtUTC: new Date(endMs).toISOString(),
    windowHours: window,
  };
}

/** Display the UTC ISO as "HH:MM MDT" for the canvas grid (America/Denver
 *  during DST = UTC-6). */
export function toMDTLabel(iso: string): string {
  return new Date(iso).toLocaleString('en-US', {
    timeZone: 'America/Denver',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  });
}

/** Format MMDDYYYY date prefix used in deploy_may12_mature.DATE_PREFIX. */
export function datePrefixFromSendDate(sendDate: string): string {
  const [yyyy, mm, dd] = sendDate.split('-').map(s => parseInt(s, 10));
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(mm)}${pad(dd)}${yyyy}`;
}
