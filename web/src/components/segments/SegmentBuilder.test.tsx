import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import {
  ConditionGroupEditor,
  DEFAULT_FIELDS,
  type ConditionGroupBuilder,
} from './SegmentBuilder';

const createGroup = (): ConditionGroupBuilder => ({
  id: 'root',
  logic_operator: 'AND',
  is_negated: false,
  conditions: [],
  groups: [],
});

describe('ConditionGroupEditor', () => {
  it('adds a condition without submitting the parent form', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn((event: SubmitEvent) => event.preventDefault());
    const onChange = vi.fn();

    render(
      <form onSubmit={onSubmit}>
        <ConditionGroupEditor
          group={createGroup()}
          fields={DEFAULT_FIELDS}
          events={['email_sent']}
          depth={0}
          onChange={onChange}
        />
      </form>
    );

    await user.click(screen.getByRole('button', { name: /add condition/i }));

    expect(onSubmit).not.toHaveBeenCalled();
    expect(onChange).toHaveBeenCalledTimes(1);

    const nextGroup = onChange.mock.calls[0][0] as ConditionGroupBuilder;
    expect(nextGroup.conditions).toHaveLength(1);
  });
});
