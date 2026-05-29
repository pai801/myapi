import { render, screen } from '@testing-library/react';
import DetailDialog from './DetailDialog';

describe('DetailDialog', () => {
  const fullLogItem = {
    channel_name: 'ch1',
    request_header: '{"Authorization":"Bearer sk-xxx"}',
    request_body: '{"model":"gpt4","messages":[{"role":"user","content":"hello"}]}',
    response_body: '{"id":"resp1","choices":[{"text":"hi"}]}'
  };

  const minimalLogItem = {
    channel_name: 'ch1'
  };

  it('renders with all fields', () => {
    render(<DetailDialog open={true} onClose={jest.fn()} logItem={fullLogItem} />);
    expect(screen.getByText('详情')).toBeInTheDocument();
    expect(screen.getByText('ch1')).toBeInTheDocument();
    expect(screen.getByText('"model": "gpt4"', { exact: false })).toBeInTheDocument();
    expect(screen.getByText('"id": "resp1"', { exact: false })).toBeInTheDocument();
    expect(screen.getByText('Bearer sk-xxx', { exact: false })).toBeInTheDocument();
  });

  it('hides empty fields', () => {
    render(<DetailDialog open={true} onClose={jest.fn()} logItem={minimalLogItem} />);
    expect(screen.getByText('ch1')).toBeInTheDocument();
    expect(screen.queryByText(/request_header/i)).toBeNull();
    expect(screen.queryByText(/request_body/i)).toBeNull();
    expect(screen.queryByText(/response_body/i)).toBeNull();
  });

  it('does not render when closed', () => {
    render(<DetailDialog open={false} onClose={jest.fn()} logItem={minimalLogItem} />);
    expect(screen.queryByText('详情')).not.toBeInTheDocument();
  });
});
