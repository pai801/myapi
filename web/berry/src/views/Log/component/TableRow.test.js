import { render, screen, fireEvent } from '@testing-library/react';
import LogTableRow from './TableRow';

describe('LogTableRow', () => {
  const mockItem = {
    id: 123,
    created_at: 1700000000,
    channel: 1,
    username: 'testuser',
    token_name: 'test-token',
    type: 2,
    model_name: 'gpt-4',
    prompt_tokens: 10,
    completion_tokens: 20,
    quota: 1000,
    content: 'some content'
  };

  it('should render Detail button when userIsAdmin is true', () => {
    render(<LogTableRow item={mockItem} userIsAdmin={true} onDetailClick={() => {}} />);
    expect(screen.getByText('详情')).toBeInTheDocument();
  });

  it('should NOT render Detail button when userIsAdmin is false', () => {
    render(<LogTableRow item={mockItem} userIsAdmin={false} />);
    expect(screen.queryByText('详情')).not.toBeInTheDocument();
  });

  it('should call onDetailClick with item when Detail button is clicked', () => {
    const onDetailClick = jest.fn();
    render(<LogTableRow item={mockItem} userIsAdmin={true} onDetailClick={onDetailClick} />);
    fireEvent.click(screen.getByText('详情'));
    expect(onDetailClick).toHaveBeenCalledWith(mockItem);
  });
});