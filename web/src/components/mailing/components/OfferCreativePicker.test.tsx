import { describe, it, expect } from 'vitest';
import { proofMatchesBrand, type OfferProof } from './OfferCreativePicker';

const proof = (domains: string[] | null): OfferProof => ({
  id: 'p', name: 'n', offer_key: 'k', approval_status: 'approved', is_active: true,
  variants: [], from_names: [], approved_domains: domains, approved_isps: [],
});

describe('proofMatchesBrand', () => {
  // Proofs record approved_domains in em.<apex> form; the board mails the SES
  // tenant m.<apex>. Both must resolve against the same brand root or the
  // picker shows an empty library for every SES-routed property.
  it('matches a proof approved on em.<apex> when sending from m.<apex>', () => {
    expect(proofMatchesBrand(proof(['em.discountblog.com']), 'discountblog.com')).toBe(true);
  });

  it('matches the bare apex', () => {
    expect(proofMatchesBrand(proof(['discountblog.com']), 'discountblog.com')).toBe(true);
  });

  it('is case-insensitive', () => {
    expect(proofMatchesBrand(proof(['EM.DiscountBlog.com']), 'discountblog.com')).toBe(true);
  });

  it('does NOT match a different property', () => {
    expect(proofMatchesBrand(proof(['em.quizfiesta.com']), 'discountblog.com')).toBe(false);
  });

  it('does NOT match a look-alike suffix', () => {
    // "notdiscountblog.com" ends with "discountblog.com" as a STRING but is a
    // different domain — the dot-boundary check is what stops it.
    expect(proofMatchesBrand(proof(['em.notdiscountblog.com']), 'discountblog.com')).toBe(false);
  });

  it('treats an empty domain list as cleared everywhere', () => {
    expect(proofMatchesBrand(proof([]), 'discountblog.com')).toBe(true);
    expect(proofMatchesBrand(proof(null), 'discountblog.com')).toBe(true);
  });

  it('does not filter when the brand root is unknown', () => {
    expect(proofMatchesBrand(proof(['em.quizfiesta.com']), '')).toBe(true);
  });
});
