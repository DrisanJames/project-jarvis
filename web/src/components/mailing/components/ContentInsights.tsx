import { useState, useEffect } from 'react';

interface ContentPattern {
  subject_style: string;
  layout_style: string;
  cta_style: string;
  tone: string;
  avg_open_rate: number;
  avg_click_rate: number;
  total_samples: number;
  campaign_count: number;
  wins: number;
}

export default function ContentInsights() {
  const [patterns, setPatterns] = useState<ContentPattern[]>([]);
  const [recommendation, setRecommendation] = useState<ContentPattern | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      fetch('/api/v1/content-learnings').then(r => r.json()),
      fetch('/api/v1/content-learnings/recommend').then(r => r.json()),
    ]).then(([pats, rec]) => {
      setPatterns(Array.isArray(pats) ? pats : []);
      setRecommendation(rec?.avg_open_rate ? rec : null);
    }).finally(() => setLoading(false));
  }, []);

  if (loading) {
    return <div className="text-gray-400 text-center py-8">Loading content learnings...</div>;
  }

  return (
    <div className="space-y-6">
      {recommendation && (
        <div className="bg-gradient-to-r from-indigo-900/50 to-purple-900/50 rounded-xl p-5 border border-indigo-700/50">
          <h3 className="text-white font-semibold mb-2 flex items-center gap-2">
            <span className="text-lg">🏆</span> Top Performing Pattern
          </h3>
          <div className="grid grid-cols-2 md:grid-cols-4 gap-3 text-sm">
            {recommendation.subject_style && (
              <div>
                <span className="text-gray-400 text-xs">Subject Style</span>
                <p className="text-white">{recommendation.subject_style}</p>
              </div>
            )}
            {recommendation.tone && (
              <div>
                <span className="text-gray-400 text-xs">Tone</span>
                <p className="text-white">{recommendation.tone}</p>
              </div>
            )}
            <div>
              <span className="text-gray-400 text-xs">Avg Open Rate</span>
              <p className="text-emerald-400 font-semibold">{(recommendation.avg_open_rate * 100).toFixed(1)}%</p>
            </div>
            <div>
              <span className="text-gray-400 text-xs">Avg Click Rate</span>
              <p className="text-purple-400 font-semibold">{(recommendation.avg_click_rate * 100).toFixed(1)}%</p>
            </div>
          </div>
        </div>
      )}

      <div className="bg-gray-800/50 rounded-xl border border-gray-700/50 overflow-hidden">
        <div className="px-5 py-3 border-b border-gray-700/50">
          <h3 className="text-white font-semibold">Content Pattern Learnings</h3>
        </div>
        {patterns.length === 0 ? (
          <div className="p-5 text-gray-500 text-center text-sm">
            No A/B test results yet. Run campaigns with content variants to build learnings.
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="text-gray-400 text-xs uppercase tracking-wider border-b border-gray-700/50">
                  <th className="px-5 py-3 text-left">Subject</th>
                  <th className="px-5 py-3 text-left">Tone</th>
                  <th className="px-5 py-3 text-right">Open Rate</th>
                  <th className="px-5 py-3 text-right">Click Rate</th>
                  <th className="px-5 py-3 text-right">Samples</th>
                  <th className="px-5 py-3 text-right">Wins</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-700/30">
                {patterns.map((p, i) => (
                  <tr key={i} className="hover:bg-gray-700/20 transition-colors">
                    <td className="px-5 py-3 text-white">{p.subject_style || '-'}</td>
                    <td className="px-5 py-3 text-gray-300">{p.tone || '-'}</td>
                    <td className="px-5 py-3 text-right text-emerald-400">{(p.avg_open_rate * 100).toFixed(1)}%</td>
                    <td className="px-5 py-3 text-right text-purple-400">{(p.avg_click_rate * 100).toFixed(1)}%</td>
                    <td className="px-5 py-3 text-right text-gray-400">{p.total_samples.toLocaleString()}</td>
                    <td className="px-5 py-3 text-right">
                      <span className="text-yellow-400">{p.wins}</span>
                      <span className="text-gray-500">/{p.campaign_count}</span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}
