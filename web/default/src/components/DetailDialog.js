import PropTypes from 'prop-types';

import {
  Modal,
  Button,
  Header,
  Segment,
} from 'semantic-ui-react';
import { copy, showSuccess, showWarning } from '../helpers';

function formatJSON(value) {
  if (!value) return null;
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}

function DetailField({ label, value, onCopy }) {
  return (
    <div style={{ marginBottom: '1em' }}>
      <Header sub>
        {label}
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
        style={{
          fontFamily: 'monospace',
          fontSize: '0.8rem',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
        }}
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
    fields.push({ label: '响应体', value: formattedResponse, copyable: true });
  }

  const formattedBody = formatJSON(logItem.request_body);
  if (formattedBody) {
    fields.push({ label: '请求体', value: formattedBody, copyable: true });
  }

  return (
    <Modal open={open} onClose={onClose} size='large'>
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
