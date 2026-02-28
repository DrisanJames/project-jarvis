
interface HeatmapCell {
  x: number;
  y: number;
  value: number;
  label?: string;
}

interface HeatmapProps {
  data: HeatmapCell[];
  xLabels: string[];
  yLabels: string[];
  colorScale?: { min: string; max: string };
  minValue?: number;
  maxValue?: number;
  cellSize?: number;
  showValues?: boolean;
  valueFormatter?: (value: number) => string;
  onCellClick?: (cell: HeatmapCell) => void;
  title?: string;
}

export function Heatmap({
  data,
  xLabels,
  yLabels,
  colorScale = { min: '#e2e8f0', max: '#48bb78' },
  minValue,
  maxValue,
  cellSize = 40,
  showValues = true,
  valueFormatter = (v) => `${(v * 100).toFixed(1)}%`,
  onCellClick,
  title,
}: HeatmapProps) {
  // Calculate min/max if not provided
  const values = data.map((d) => d.value);
  const min = minValue ?? Math.min(...values);
  const max = maxValue ?? Math.max(...values);

  // Create a lookup map for cells
  const cellMap = new Map<string, HeatmapCell>();
  data.forEach((cell) => {
    cellMap.set(`${cell.x}-${cell.y}`, cell);
  });

  // Interpolate color
  const getColor = (value: number): string => {
    if (max === min) return colorScale.max;
    const ratio = (value - min) / (max - min);
    
    // Parse colors
    const parseColor = (color: string) => {
      const match = color.match(/^#([a-f\d]{2})([a-f\d]{2})([a-f\d]{2})$/i);
      if (match) {
        return {
          r: parseInt(match[1], 16),
          g: parseInt(match[2], 16),
          b: parseInt(match[3], 16),
        };
      }
      return { r: 200, g: 200, b: 200 };
    };

    const minColor = parseColor(colorScale.min);
    const maxColor = parseColor(colorScale.max);

    const r = Math.round(minColor.r + ratio * (maxColor.r - minColor.r));
    const g = Math.round(minColor.g + ratio * (maxColor.g - minColor.g));
    const b = Math.round(minColor.b + ratio * (maxColor.b - minColor.b));

    return `rgb(${r}, ${g}, ${b})`;
  };

  // Determine text color based on background
  const getTextColor = (value: number): string => {
    const ratio = (value - min) / (max - min);
    return ratio > 0.5 ? '#fff' : '#1a202c';
  };

  return (
    <div className="heatmap-container">
      {title && <h4 style={{ margin: '0 0 1rem 0' }}>{title}</h4>}
      <div style={{ overflowX: 'auto' }}>
        <table style={{ borderCollapse: 'collapse' }}>
          <thead>
            <tr>
              <th style={{ width: cellSize * 2 }}></th>
              {xLabels.map((label, i) => (
                <th
                  key={i}
                  style={{
                    width: cellSize,
                    height: cellSize,
                    fontSize: '0.75rem',
                    fontWeight: 500,
                    textAlign: 'center',
                  }}
                >
                  {label}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {yLabels.map((yLabel, y) => (
              <tr key={y}>
                <td
                  style={{
                    fontSize: '0.75rem',
                    fontWeight: 500,
                    textAlign: 'right',
                    paddingRight: '0.5rem',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {yLabel}
                </td>
                {xLabels.map((_, x) => {
                  const cell = cellMap.get(`${x}-${y}`);
                  const value = cell?.value ?? 0;
                  const bgColor = cell ? getColor(value) : '#f7fafc';
                  const textColor = cell ? getTextColor(value) : '#a0aec0';

                  return (
                    <td
                      key={x}
                      onClick={() => cell && onCellClick?.(cell)}
                      style={{
                        width: cellSize,
                        height: cellSize,
                        backgroundColor: bgColor,
                        color: textColor,
                        textAlign: 'center',
                        fontSize: '0.625rem',
                        cursor: onCellClick && cell ? 'pointer' : 'default',
                        border: '1px solid var(--border-color, #e2e8f0)',
                        transition: 'transform 0.1s',
                      }}
                      title={cell?.label || (cell ? valueFormatter(value) : 'No data')}
                    >
                      {showValues && cell ? valueFormatter(value) : ''}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {/* Legend */}
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          marginTop: '1rem',
          gap: '0.5rem',
          fontSize: '0.75rem',
        }}
      >
        <span>Low</span>
        <div
          style={{
            width: 100,
            height: 12,
            background: `linear-gradient(to right, ${colorScale.min}, ${colorScale.max})`,
            borderRadius: 4,
          }}
        />
        <span>High</span>
      </div>
    </div>
  );
}

export default Heatmap;
