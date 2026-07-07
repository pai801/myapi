import { useState } from 'react';
import PropTypes from 'prop-types';

import {
  Modal,
  Button,
  Header,
  Segment,
} from 'semantic-ui-react';
import { copy, isMobile, showSuccess, showWarning } from '../helpers';

function formatJSON(value) {
  if (!value) return null;
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}

function DetailField({ label, value, onCopy, collapsible }) {
  const [expanded, setExpanded] = useState(true);
  const isLong = value.split('\n').length > 5;
  const canCollapse = collapsible && isLong;

  const segmentStyle = {
    fontFamily: 'monospace',
    fontSize: isMobile() ? '0.75rem' : '0.8rem',
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-all',
    ...(canCollapse && !expanded ? { maxHeight: isMobile() ? 200 : 300, overflow: 'auto' } : {}),
  };

  return (
    <div style={{ marginBottom: '1em' }}>
      <Header sub>
        {label}
        {canCollapse && (
          <Button
            size='mini'
            compact
            floated='right'
            onClick={() => setExpanded(!expanded)}
            style={{ marginLeft: '0.5em' }}
            aria-expanded={expanded}
          >
            {expanded ? '收起' : '展开'}
          </Button>
        )}
        {onCopy && (
          <Button
            size='mini'
            compact
            floated='right'
            onClick={onCopy}
            style={{ marginLeft: '0.5em' }}
          >
            复制
          </Button>
        )}
      </Header>
      <Segment
        secondary
        style={segmentStyle}
        tabIndex={canCollapse && !expanded ? 0 : undefined}
      >
        {value}
      </Segment>
    </div>
  );
}

DetailField.propTypes = {
  label: PropTypes.string.isRequired,
  value: PropTypes.string.isRequired,
  onCopy: PropTypes.func,
  collapsible: PropTypes.bool,
};

export default function DetailDialog({ open, onClose, logItem }) {
  const fields = [];

  if (!logItem) return null;

  if (logItem.channel_name) {
    fields.push({ label: '渠道名称', value: logItem.channel_name });
  }

  const formattedHeader = formatJSON(logItem.request_header);
  if (formattedHeader) {
    fields.push({ label: '请求头', value: formattedHeader });
  }

  const handleCopy = async (text) => {
    if (await copy(text)) {
      showSuccess('已复制');
    } else {
      showWarning('复制失败');
    }
  };

  const formattedResponse = formatJSON(logItem.response_body);
  if (formattedResponse) {
    fields.push({ label: '响应体', value: formattedResponse, copyable: true, collapsible: true });
  }

  const formattedBody = formatJSON(logItem.request_body);
  if (formattedBody) {
    fields.push({ label: '请求体', value: formattedBody, copyable: true, collapsible: true });
  }

  return (
    <Modal
      key={logItem?.id ?? 'empty'}
      open={open}
      onClose={onClose}
      size={isMobile() ? 'small' : 'large'}
      className='log-detail-modal'
    >
      <Modal.Header>
        详情
      </Modal.Header>
      <Modal.Content scrolling>
        {fields.map((field, index) => (
          <DetailField
            key={index}
            label={field.label}
            value={field.value}
            onCopy={field.copyable ? () => handleCopy(field.value) : undefined}
            collapsible={field.collapsible}
          />
        ))}
      </Modal.Content>
      <Modal.Actions>
        <Button onClick={onClose}>关闭</Button>
      </Modal.Actions>
    </Modal>
  );
}

DetailDialog.propTypes = {
  open: PropTypes.bool.isRequired,
  onClose: PropTypes.func.isRequired,
  logItem: PropTypes.object,
};
