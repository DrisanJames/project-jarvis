import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { Card, CardHeader, CardBody, CardFooter } from './Card';

describe('Card', () => {
  it('renders children correctly', () => {
    render(
      <Card>
        <div data-testid="child">Content</div>
      </Card>
    );
    expect(screen.getByTestId('child')).toBeInTheDocument();
  });

  it('applies custom className', () => {
    render(<Card className="custom-class">Content</Card>);
    const card = screen.getByText('Content').closest('.card');
    expect(card).toHaveClass('card');
    expect(card).toHaveClass('custom-class');
  });
});

describe('CardHeader', () => {
  it('renders title', () => {
    render(<CardHeader title="Test Title" />);
    expect(screen.getByText('Test Title')).toBeInTheDocument();
  });

  it('renders action when provided', () => {
    render(
      <CardHeader 
        title="Test Title" 
        action={<button>Action</button>}
      />
    );
    expect(screen.getByText('Action')).toBeInTheDocument();
  });
});

describe('CardBody', () => {
  it('renders children', () => {
    render(
      <CardBody>
        <p>Body content</p>
      </CardBody>
    );
    expect(screen.getByText('Body content')).toBeInTheDocument();
  });

  it('applies custom className', () => {
    render(<CardBody className="custom-body">Content</CardBody>);
    const body = screen.getByText('Content');
    expect(body).toHaveClass('card-body', 'custom-body');
  });
});

describe('CardFooter', () => {
  it('renders children', () => {
    render(
      <CardFooter>
        <p>Footer content</p>
      </CardFooter>
    );
    expect(screen.getByText('Footer content')).toBeInTheDocument();
  });
});
