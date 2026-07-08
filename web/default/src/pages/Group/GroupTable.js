import React, { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Confirm,
  Icon,
  Pagination,
  Table,
} from 'semantic-ui-react';
import { API, showError, showSuccess } from '../../helpers';
import { ITEMS_PER_PAGE } from '../../constants';
import EditGroup from './EditGroup';

const GroupTable = () => {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [items, setItems] = useState([]);
  const [total, setTotal] = useState(0);
  const [activePage, setActivePage] = useState(1);
  const [modalOpen, setModalOpen] = useState(false);
  const [editingGroup, setEditingGroup] = useState(null);
  const [deleteId, setDeleteId] = useState(null);

  const loadGroups = useCallback(
    async (page) => {
      setLoading(true);
      try {
        const res = await API.get(
          `/api/group/list?p=${page - 1}`
        );
        const { success, message, data } = res.data;
        if (success) {
          setItems(data.items || []);
          setTotal(data.total || 0);
        } else {
          showError(message);
        }
      } catch (error) {
        showError(error.message);
      }
      setLoading(false);
    },
    []
  );

  useEffect(() => {
    loadGroups(activePage).then();
  }, [activePage, loadGroups]);

  const onPaginationChange = (e, { activePage }) => {
    setActivePage(activePage);
  };

  const handleAdd = () => {
    setEditingGroup(null);
    setModalOpen(true);
  };

  const handleEdit = (group) => {
    setEditingGroup(group);
    setModalOpen(true);
  };

  const handleDelete = (group) => {
    setDeleteId(group.id);
  };

  const confirmDelete = async () => {
    try {
      const res = await API.delete(`/api/group/${deleteId}`);
      const { success, message } = res.data;
      if (success) {
        showSuccess(t('token.edit.messages.operation_success'));
        if (items.length === 1 && activePage > 1) {
          setActivePage(activePage - 1);
        } else {
          loadGroups(activePage).then();
        }
      } else {
        showError(message);
      }
    } catch (error) {
      showError(error.message);
    }
    setDeleteId(null);
  };

  const handleSaved = () => {
    setModalOpen(false);
    loadGroups(activePage).then();
  };

  const totalPages = Math.max(1, Math.ceil(total / ITEMS_PER_PAGE));

  return (
    <div>
      <Table basic='very' celled selectable compact>
        <Table.Header>
          <Table.Row>
            <Table.HeaderCell>#</Table.HeaderCell>
            <Table.HeaderCell>{t('group.name')}</Table.HeaderCell>
            <Table.HeaderCell>{t('group.model_ratio')}</Table.HeaderCell>
            <Table.HeaderCell>{t('channel.table.actions')}</Table.HeaderCell>
          </Table.Row>
        </Table.Header>
        <Table.Body>
          {items.map((group, idx) => (
            <Table.Row key={group.id}>
              <Table.Cell>{(activePage - 1) * ITEMS_PER_PAGE + idx + 1}</Table.Cell>
              <Table.Cell>{group.name}</Table.Cell>
              <Table.Cell>
                {group.model_ratio !== undefined ? parseFloat(group.model_ratio).toFixed(2) : '1.00'}
              </Table.Cell>
              <Table.Cell>
                <Button
                  size='tiny'
                  onClick={() => handleEdit(group)}
                  style={{ marginRight: 8 }}
                >
                  <Icon name='edit' /> {t('group.edit')}
                </Button>
                <Button
                  size='tiny'
                  negative
                  onClick={() => handleDelete(group)}
                >
                  <Icon name='trash' /> {t('token.buttons.delete')}
                </Button>
              </Table.Cell>
            </Table.Row>
          ))}
          {items.length === 0 && !loading && (
            <Table.Row>
              <Table.Cell colSpan='4' textAlign='center'>
                {t('channel.table.no_name')}
              </Table.Cell>
            </Table.Row>
          )}
        </Table.Body>
        <Table.Footer>
          <Table.Row>
            <Table.HeaderCell colSpan='4'>
              <Button
                size='tiny'
                positive
                onClick={handleAdd}
                style={{ marginRight: 8 }}
              >
                <Icon name='add' /> {t('group.add')}
              </Button>
              <Pagination
                floated='right'
                activePage={activePage}
                onPageChange={onPaginationChange}
                size='tiny'
                siblingRange={1}
                totalPages={totalPages}
              />
            </Table.HeaderCell>
          </Table.Row>
        </Table.Footer>
      </Table>
      <EditGroup
        open={modalOpen}
        group={editingGroup}
        onClose={() => setModalOpen(false)}
        onSaved={handleSaved}
      />
      <Confirm
        open={deleteId !== null}
        content={t('group.delete_confirm')}
        confirmButton={t('token.buttons.delete')}
        cancelButton={t('token.edit.buttons.cancel')}
        onCancel={() => {
          setDeleteId(null);
        }}
        onConfirm={confirmDelete}
      />
    </div>
  );
};

export default GroupTable;
