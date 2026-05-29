import { render, screen } from '@testing-library/react';
import LogTableHead from './TableHead';

describe('LogTableHead', () => {
  it('should render detail column when userIsAdmin is true', () => {
    render(<LogTableHead userIsAdmin={true} />);
    const detailHeader = screen.getByText('详情');
    expect(detailHeader).toBeInTheDocument();
  });

  it('should NOT render detail column when userIsAdmin is false', () => {
    render(<LogTableHead userIsAdmin={false} />);
    const detailHeader = screen.queryByText('详情');
    expect(detailHeader).not.toBeInTheDocument();
  });
});