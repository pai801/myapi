import React, { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Confirm, Icon, Label, Table } from 'semantic-ui-react';
import { API, showError, showSuccess } from '../../helpers';
import EditMetadata from './EditMetadata';

const MetadataTable = () => {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState([]);
  const [modalOpen, setModalOpen] = useState(false);
  const [editingMetadata, setEditingMetadata] = useState(null);
  const [deleteName, setDeleteName] = useState(null);

  // 后端列表接口一次性返回全量数组，无分页参数
  const loadMetadata = useCallback(async () => {
    setLoading(true);
    try {
      const res = await API.get('/api/model-metadata/');
      const { success, message, data } = res.data;
      if (success) {
        setItems(data || []);
      } else {
        showError(message);
      }
    } catch (error) {
      showError(error.message);
    }
    setLoading(false);
  }, []);

  useEffect(() => {
    loadMetadata().then();
  }, [loadMetadata]);

  const handleAdd = () => {
    setEditingMetadata(null);
    setModalOpen(true);
  };

  const handleEdit = (metadata) => {
    setEditingMetadata(metadata);
    setModalOpen(true);
  };

  const handleDelete = (metadata) => {
    setDeleteName(metadata.name);
  };

  const confirmDelete = async () => {
    try {
      const res = await API.delete(
        `/api/model-metadata/${encodeURIComponent(deleteName)}`
      );
      const { success, message } = res.data;
      if (success) {
        showSuccess(t('metadata.messages.operation_success'));
        loadMetadata().then();
      } else {
        showError(message);
      }
    } catch (error) {
      showError(error.message);
    }
    setDeleteName(null);
  };

  const handleSaved = () => {
    setModalOpen(false);
    loadMetadata().then();
  };

  const renderSupported = (supported) =>
    supported ? (
      <Label basic color='green'>
        {t('metadata.table.supported')}
      </Label>
    ) : (
      <Label basic color='grey'>
        {t('metadata.table.unsupported')}
      </Label>
    );

  return (
    <div>
      <div className='table-scroll-wrapper'>
        <Table unstackable basic={'very'} compact size='small'>
          <Table.Header>
            <Table.Row>
              <Table.HeaderCell>{t('metadata.table.name')}</Table.HeaderCell>
              <Table.HeaderCell>{t('metadata.table.display_name')}</Table.HeaderCell>
              <Table.HeaderCell>{t('metadata.table.canonical_name')}</Table.HeaderCell>
              <Table.HeaderCell>{t('metadata.table.supported_in_api')}</Table.HeaderCell>
              <Table.HeaderCell>{t('metadata.table.priority')}</Table.HeaderCell>
              <Table.HeaderCell>{t('metadata.table.context_window')}</Table.HeaderCell>
              <Table.HeaderCell>{t('metadata.table.truncation_policy')}</Table.HeaderCell>
              <Table.HeaderCell>{t('metadata.table.actions')}</Table.HeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {items.map((metadata) => (
              <Table.Row key={metadata.name}>
                <Table.Cell>{metadata.name}</Table.Cell>
                <Table.Cell>{metadata.display_name}</Table.Cell>
                <Table.Cell>{metadata.canonical_name || '-'}</Table.Cell>
                <Table.Cell>{renderSupported(metadata.supported_in_api)}</Table.Cell>
                <Table.Cell>{metadata.priority}</Table.Cell>
                <Table.Cell>{metadata.context_window}</Table.Cell>
                <Table.Cell>{metadata.truncation_policy || '-'}</Table.Cell>
                <Table.Cell>
                  <Button
                    size='tiny'
                    onClick={() => handleEdit(metadata)}
                    style={{ marginRight: 8 }}
                  >
                    <Icon name='edit' /> {t('metadata.buttons.edit')}
                  </Button>
                  <Button
                    size='tiny'
                    negative
                    onClick={() => handleDelete(metadata)}
                  >
                    <Icon name='trash' /> {t('metadata.buttons.delete')}
                  </Button>
                </Table.Cell>
              </Table.Row>
            ))}
            {items.length === 0 && !loading && (
              <Table.Row>
                <Table.Cell colSpan='8' textAlign='center'>
                  {t('metadata.table.no_data')}
                </Table.Cell>
              </Table.Row>
            )}
          </Table.Body>
        </Table>
      </div>
      <div className='table-footer-toolbar scroll-x-nowrap'>
        <Button
          size='tiny'
          positive
          onClick={handleAdd}
          loading={loading}
          style={{ marginRight: 8 }}
        >
          <Icon name='add' /> {t('metadata.buttons.add')}
        </Button>
      </div>
      <EditMetadata
        open={modalOpen}
        metadata={editingMetadata}
        onClose={() => setModalOpen(false)}
        onSaved={handleSaved}
      />
      <Confirm
        open={deleteName !== null}
        content={t('metadata.messages.delete_confirm')}
        confirmButton={t('metadata.buttons.delete')}
        cancelButton={t('metadata.buttons.cancel')}
        onCancel={() => {
          setDeleteName(null);
        }}
        onConfirm={confirmDelete}
      />
    </div>
  );
};

export default MetadataTable;
