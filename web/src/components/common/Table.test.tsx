import { render, screen, fireEvent } from '@testing-library/react';
import { describe, it, expect, vi } from 'vitest';
import { Table } from './Table';

interface TestItem {
  id: string;
  name: string;
  value: number;
}

describe('Table', () => {
  const testData: TestItem[] = [
    { id: '1', name: 'Item 1', value: 100 },
    { id: '2', name: 'Item 2', value: 200 },
    { id: '3', name: 'Item 3', value: 300 },
  ];

  const columns = [
    { key: 'name', header: 'Name' },
    { key: 'value', header: 'Value', align: 'right' as const },
  ];

  it('renders table headers', () => {
    render(
      <Table
        columns={columns}
        data={testData}
        keyExtractor={(item) => item.id}
      />
    );
    expect(screen.getByText('Name')).toBeInTheDocument();
    expect(screen.getByText('Value')).toBeInTheDocument();
  });

  it('renders table rows', () => {
    render(
      <Table
        columns={columns}
        data={testData}
        keyExtractor={(item) => item.id}
      />
    );
    expect(screen.getByText('Item 1')).toBeInTheDocument();
    expect(screen.getByText('Item 2')).toBeInTheDocument();
    expect(screen.getByText('Item 3')).toBeInTheDocument();
  });

  it('renders custom cell content using render function', () => {
    const columnsWithRender = [
      { 
        key: 'name', 
        header: 'Name',
        render: (item: TestItem) => <strong>{item.name}</strong>,
      },
    ];

    render(
      <Table
        columns={columnsWithRender}
        data={testData}
        keyExtractor={(item) => item.id}
      />
    );
    
    const strongElements = screen.getAllByText(/Item/);
    expect(strongElements[0].tagName).toBe('STRONG');
  });

  it('shows empty message when no data', () => {
    render(
      <Table
        columns={columns}
        data={[] as TestItem[]}
        keyExtractor={(item) => item.id}
        emptyMessage="No items found"
      />
    );
    expect(screen.getByText('No items found')).toBeInTheDocument();
  });

  it('calls onRowClick when row is clicked', () => {
    const handleClick = vi.fn();
    
    render(
      <Table
        columns={columns}
        data={testData}
        keyExtractor={(item) => item.id}
        onRowClick={handleClick}
      />
    );
    
    fireEvent.click(screen.getByText('Item 1'));
    expect(handleClick).toHaveBeenCalledWith(testData[0]);
  });

  it('handles nested key access', () => {
    const nestedData = [
      { id: '1', nested: { value: 'nested value' } },
    ];
    const nestedColumns = [
      { key: 'nested.value', header: 'Nested' },
    ];

    render(
      <Table
        columns={nestedColumns}
        data={nestedData}
        keyExtractor={(item) => item.id}
      />
    );
    expect(screen.getByText('nested value')).toBeInTheDocument();
  });
});
