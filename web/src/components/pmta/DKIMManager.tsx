import React, { useState, useCallback } from 'react';
import { useApi, useApiMutation } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

interface DKIMKey {
  id: string;
  selector: string;
  algorithm: string;
  key_size: number;
  dns_record_value: string;
  dns_record: string;
  dns_verified: boolean;
  active: boolean;
  created_at: string;
}

interface DKIMData {
  domain: string;
  keys: DKIMKey[];
}

interface Props {
  domainId: string;
  domainName: string;
}

const DKIMManager: React.FC<Props> = ({ domainId, domainName }) => {
  const [selector, setSelector] = useState('s1');
  const [keySize, setKeySize] = useState(2048);

  const { data, loading, refetch } = useApi<DKIMData>(`/api/mailing/domains/${domainId}/dkim`);
  const { mutate: generate, loading: generating } = useApiMutation<
    { selector: string; key_size: number },
    { id: string; dns_record: string; dns_value: string }
  >(`/api/mailing/domains/${domainId}/dkim/generate`);

  const handleGenerate = useCallback(async () => {
    await generate({ selector, key_size: keySize });
    refetch();
  }, [generate, selector, keySize, refetch]);

  const handleVerify = useCallback(async () => {
    await fetch(`/api/mailing/domains/${domainId}/dkim/verify`, {
      method: 'POST', credentials: 'include',
    });
    refetch();
  }, [domainId, refetch]);

  if (loading && !data) return <Loading />;

  const keys = data?.keys ?? [];

  return (
    <Card>
      <CardHeader>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span>DKIM Keys — {domainName}</span>
          <button
            onClick={handleVerify}
            style={{ padding: '0.3rem 0.6rem', background: '#374151', color: '#d1d5db', border: 'none', borderRadius: '0.25rem', cursor: 'pointer', fontSize: '0.8rem' }}
          >
            Verify DNS
          </button>
        </div>
      </CardHeader>
      <CardBody>
        {/* Generate new key */}
        <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '1rem', alignItems: 'center' }}>
          <input
            value={selector}
            onChange={e => setSelector(e.target.value)}
            placeholder="Selector (e.g. s1)"
            style={{ padding: '0.4rem', background: '#111827', border: '1px solid #374151', borderRadius: '0.375rem', color: '#f3f4f6', width: '120px', fontSize: '0.85rem' }}
          />
          <select
            value={keySize}
            onChange={e => setKeySize(Number(e.target.value))}
            style={{ padding: '0.4rem', background: '#111827', border: '1px solid #374151', borderRadius: '0.375rem', color: '#f3f4f6', fontSize: '0.85rem' }}
          >
            <option value={1024}>1024-bit</option>
            <option value={2048}>2048-bit</option>
          </select>
          <button
            onClick={handleGenerate}
            disabled={generating}
            style={{ padding: '0.4rem 0.8rem', background: '#4f46e5', color: '#fff', border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.85rem' }}
          >
            {generating ? 'Generating...' : 'Generate DKIM Key'}
          </button>
        </div>

        {/* Existing keys */}
        {keys.length > 0 ? (
          <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
            {keys.map(key => (
              <div key={key.id} style={{ padding: '0.75rem', background: '#1f2937', borderRadius: '0.5rem' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem' }}>
                  <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                    <span style={{ fontFamily: 'monospace', fontWeight: 600 }}>{key.selector}._domainkey.{domainName}</span>
                    <StatusBadge
                      status={key.dns_verified ? 'healthy' : 'warning'}
                      label={key.dns_verified ? 'Verified' : 'Pending'}
                    />
                  </div>
                  <span style={{ fontSize: '0.75rem', color: '#9ca3af' }}>
                    {key.algorithm} / {key.key_size}-bit
                  </span>
                </div>
                <div style={{ fontSize: '0.75rem', color: '#9ca3af', marginBottom: '0.25rem' }}>DNS TXT Value:</div>
                <div style={{
                  padding: '0.5rem', background: '#111827', borderRadius: '0.25rem',
                  fontFamily: 'monospace', fontSize: '0.7rem', wordBreak: 'break-all',
                  color: '#d1d5db', maxHeight: '60px', overflow: 'auto',
                }}>
                  {key.dns_record_value}
                </div>
              </div>
            ))}
          </div>
        ) : (
          <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1rem' }}>
            No DKIM keys generated for this domain.
          </p>
        )}
      </CardBody>
    </Card>
  );
};

export default DKIMManager;
