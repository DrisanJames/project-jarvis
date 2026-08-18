// OfferCreativePreview.test.tsx — regression guard for the blank Preview
// (operator 2026-08-18: "whenever I select the copy that I want to send, and
// then I click preview to see the copy, it does not display").
//
// ROOT CAUSE the wizard shipped with: the picker's load effect keys off the
// `orgFetch` prop, and the wizard passed an INLINE ARROW. Selecting a proof
// calls onApply → the wizard re-renders → a new arrow identity → the effect
// re-fires → setProofs(<the LIST rows>). The list endpoint deliberately omits
// html_content (offer_proofs.go scanProof(rows, false)), so the cached body was
// wiped and the modal opened an empty iframe while the Preview button stayed
// enabled (hasHtml reads the WIZARD's variant, which still held the html).
//
// The fix: preview `currentHtml` — the html the wizard will actually ship —
// rather than the re-fetchable list row. These tests pin that behavior.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { OfferCreativePicker } from './OfferCreativePicker';

const LIST_ROW = {
  id: 'proof-1',
  name: 'Globe Life - New Form v1',
  offer_key: 'GLNFv1',
  approval_status: 'approved',
  is_active: true,
  variants: [{ subject: 'Coverage starting at $9.95', preheader: 'no exam' }],
  from_names: ['Globe Life Offer'],
  approved_domains: ['em.financialcalculate.com'],
  approved_isps: [],
  // NOTE: no html_content — exactly what GET /offer-proofs returns.
};

const SHIPPING_HTML = '<html><body><h1>Globe Life</h1></body></html>';

const makeFetch = () =>
  vi.fn(async (url: string) => {
    if (url.includes('/offer-proofs?')) {
      return { ok: true, json: async () => ({ proofs: [LIST_ROW] }) } as Response;
    }
    if (url.includes('/offer-suppression')) {
      return {
        ok: true,
        json: async () => ({
          offer_id: 'o1', offer_name: 'Globe Life - New Form', offer_status: 'active',
          suppression_count: 0, reasons: [], suggested_lists: [], siblings: [],
          warning: 'This offer has NO suppression of any kind on file — no offer-level ledger and no advertiser list. Nothing will be subtracted for this advertiser.',
        }),
      } as Response;
    }
    return { ok: true, json: async () => ({}) } as Response;
  });

const renderPicker = (overrides: Record<string, unknown> = {}) =>
  render(
    <OfferCreativePicker
      apiBase="/api/mailing"
      orgFetch={makeFetch() as never}
      sendingDomain="em.financialcalculate.com"
      brandRoot="financialcalculate.com"
      offers={[{ id: 'o1', name: 'Globe Life - New Form', status: 'active' }]}
      offersError=""
      selectedOfferId=""
      onOfferChange={() => {}}
      proofId="proof-1"
      subject="Coverage starting at $9.95"
      preheader="no exam"
      fromName="Globe Life Offer"
      hasHtml
      currentHtml={SHIPPING_HTML}
      onApply={() => {}}
      onFieldChange={() => {}}
      {...overrides}
    />,
  );

describe('OfferCreativePicker preview', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders the html the wizard will SHIP even when the cached proof row has none', async () => {
    renderPicker();
    // Wait for the proofs list (which carries no html_content) to land.
    await screen.findByText('Globe Life - New Form v1');

    fireEvent.click(screen.getByRole('button', { name: /preview/i }));

    const frame = await screen.findByTitle('creative preview');
    expect(frame.getAttribute('srcdoc')).toBe(SHIPPING_HTML);
  });

  it('says so plainly instead of showing a blank iframe when there is no body anywhere', async () => {
    renderPicker({ currentHtml: '', hasHtml: true });
    await screen.findByText('Globe Life - New Form v1');

    fireEvent.click(screen.getByRole('button', { name: /preview/i }));

    // Negative control: no iframe, and the operator is told why.
    await waitFor(() => {
      expect(screen.queryByTitle('creative preview')).toBeNull();
    });
    expect(screen.getByText(/No HTML body is loaded for this creative/i)).toBeTruthy();
  });
});

describe('OfferCreativePicker offer suppression panel', () => {
  beforeEach(() => vi.clearAllMocks());

  it('is silent until an offer is chosen', async () => {
    renderPicker();
    await screen.findByText('Globe Life - New Form v1');
    expect(screen.queryByText(/Offer suppression that will fire/i)).toBeNull();
  });

  it('reports a ZERO ledger loudly — the Globe Life case', async () => {
    renderPicker({ selectedOfferId: 'o1' });
    await screen.findByText(/Offer suppression that will fire/i);
    expect(await screen.findByText(/NO suppression of any kind on file/i)).toBeTruthy();
    // The count itself is rendered, so "0" is visible rather than implied.
    expect(screen.getByText(/subscribers suppressed for this offer/i)).toBeTruthy();
  });
});
