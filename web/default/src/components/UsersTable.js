import React, { useEffect, useState } from 'react';
import {
  Button,
  Form,
  Label,
  Pagination,
  Popup,
  Table,
} from 'semantic-ui-react';
import { Link, useNavigate, useLocation } from 'react-router-dom';
import { API, showError, showSuccess } from '../helpers';
import { useTranslation } from 'react-i18next';
import { ITEMS_PER_PAGE } from '../constants';
import { renderQuota, renderText } from '../helpers/render';

// Module-level cache to preserve user list across unmount/remount cycles
let cachedUsers = [];

const UsersTable = () => {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const location = useLocation();
  const [users, setUsers] = useState(cachedUsers);
  const [loading, setLoading] = useState(cachedUsers.length === 0);
  const [activePage, setActivePage] = useState(1);
  const [searchKeyword, setSearchKeyword] = useState('');
  const [searching, setSearching] = useState(false);

  const syncCachedUser = (updatedUser) => {
    if (!updatedUser?.id) {
      return;
    }
    cachedUsers = cachedUsers.map((user) =>
      user.id === updatedUser.id ? { ...user, ...updatedUser } : user
    );
    setUsers(cachedUsers);
  };

  const loadUsers = async (startIdx) => {
    const res = await API.get(`/api/user/?p=${startIdx}`);
    const { success, message, data } = res.data;
    if (success) {
      if (startIdx === 0) {
        setUsers(data);
        cachedUsers = data;
      } else {
        let newUsers = [...users];
        newUsers.push(...data);
        setUsers(newUsers);
        cachedUsers = newUsers;
      }
    } else {
      showError(message);
    }
    setLoading(false);
  };

  const onPaginationChange = (e, { activePage }) => {
    (async () => {
      if (activePage === Math.ceil(users.length / ITEMS_PER_PAGE) + 1) {
        await loadUsers(activePage - 1);
      }
      setActivePage(activePage);
    })();
  };

  useEffect(() => {
    // Skip reload when navigating back from edit/add cancel
    if (location.state?.skipRefresh) {
      if (location.state?.updatedUser) {
        syncCachedUser(location.state.updatedUser);
      }
      setLoading(false);
      window.history.replaceState({}, '');
      return;
    }
    loadUsers(0)
      .then()
      .catch((reason) => {
        showError(reason);
      });
  }, []);

  const manageUser = (username, action, idx) => {
    (async () => {
      const res = await API.post('/api/user/manage', {
        username,
        action,
      });
      const { success, message } = res.data;
      if (success) {
        showSuccess(t('user.messages.operation_success'));
        let user = res.data.data;
        let newUsers = [...users];
        let realIdx = (activePage - 1) * ITEMS_PER_PAGE + idx;
        newUsers[realIdx] = { ...newUsers[realIdx], ...user };
        setUsers(newUsers);
        cachedUsers = newUsers;
      } else {
        showError(message);
      }
    })();
  };

  const renderStatus = (status) => {
    switch (status) {
      case 1:
        return <Label basic>{t('user.table.status_types.activated')}</Label>;
      case 2:
        return (
          <Label basic color='red'>
            {t('user.table.status_types.banned')}
          </Label>
        );
      default:
        return (
          <Label basic color='grey'>
            {t('user.table.status_types.unknown')}
          </Label>
        );
    }
  };

  const searchUsers = async () => {
    if (searchKeyword === '') {
      await loadUsers(0);
      setActivePage(1);
      return;
    }
    setSearching(true);
    const res = await API.get(`/api/user/search?keyword=${searchKeyword}`);
    const { success, message, data } = res.data;
    if (success) {
      setUsers(data);
      setActivePage(1);
    } else {
      showError(message);
    }
    setSearching(false);
  };

  const handleKeywordChange = async (e, { value }) => {
    setSearchKeyword(value.trim());
  };

  const refreshUsers = async () => {
    setLoading(true);
    await loadUsers(0);
    setActivePage(1);
  };

  return (
    <>
      <Form onSubmit={searchUsers}>
        <Form.Input
          icon='search'
          fluid
          iconPosition='left'
          placeholder={t('user.search')}
          value={searchKeyword}
          loading={searching}
          onChange={handleKeywordChange}
        />
      </Form>

      <div className='table-scroll-wrapper'>
      <Table unstackable basic={'very'} compact size='small'>
        <Table.Header>
          <Table.Row>
            <Table.HeaderCell>{t('user.table.id')}</Table.HeaderCell>
            <Table.HeaderCell>{t('user.table.username')}</Table.HeaderCell>
            <Table.HeaderCell className='hide-on-mobile'>{t('user.edit.display_name')}</Table.HeaderCell>
            <Table.HeaderCell>{t('user.table.remaining_quota')}</Table.HeaderCell>
            <Table.HeaderCell className='hide-on-mobile'>{t('user.table.used_quota')}</Table.HeaderCell>
            <Table.HeaderCell>{t('user.table.status_text')}</Table.HeaderCell>
            <Table.HeaderCell>{t('user.table.actions')}</Table.HeaderCell>
          </Table.Row>
        </Table.Header>

        <Table.Body>
          {users
            .slice(
              (activePage - 1) * ITEMS_PER_PAGE,
              activePage * ITEMS_PER_PAGE
            )
            .map((user, idx) => {
              return (
                <Table.Row key={user.id}>
                  <Table.Cell>{user.id}</Table.Cell>
                  <Table.Cell>{renderText(user.username, 15)}</Table.Cell>
                  <Table.Cell className='hide-on-mobile'>{renderText(user.display_name, 15)}</Table.Cell>
                  <Table.Cell>
                    <Popup
                      content={t('user.table.remaining_quota')}
                      trigger={
                        <Label basic>{renderQuota(user.quota, t)}</Label>
                      }
                    />
                  </Table.Cell>
                  <Table.Cell className='hide-on-mobile'>
                    <Popup
                      content={t('user.table.used_quota')}
                      trigger={
                        <Label basic>{renderQuota(user.used_quota, t)}</Label>
                      }
                    />
                  </Table.Cell>
                  <Table.Cell>{renderStatus(user.status)}</Table.Cell>
                  <Table.Cell>
                    <div>
                      <Button
                        size={'tiny'}
                        onClick={() => {
                          manageUser(
                            user.username,
                            user.status === 1 ? 'disable' : 'enable',
                            idx
                          );
                        }}
                        disabled={user.role === 100}
                      >
                        {user.status === 1
                          ? t('user.buttons.disable')
                          : t('user.buttons.enable')}
                      </Button>
                      <Button
                        size={'tiny'}
                        onClick={() =>
                          navigate(`/user/edit/${user.id}`, { state: { user } })
                        }
                      >
                        {t('user.buttons.edit')}
                      </Button>
                    </div>
                  </Table.Cell>
                </Table.Row>
              );
            })}
        </Table.Body>

        <Table.Footer>
          <Table.Row>
            <Table.HeaderCell colSpan='7'>
              <div className='scroll-x-nowrap'>
                <Button size='small' as={Link} to='/user/add' loading={loading}>
                  {t('user.buttons.add')}
                </Button>
                <Button
                  size='small'
                  icon='refresh'
                  onClick={refreshUsers}
                  loading={loading}
                />
                <Pagination
                  floated='right'
                  activePage={activePage}
                  onPageChange={onPaginationChange}
                  size='small'
                  siblingRange={1}
                  totalPages={
                    Math.ceil(users.length / ITEMS_PER_PAGE) +
                    (users.length % ITEMS_PER_PAGE === 0 ? 1 : 0)
                  }
                />
              </div>
            </Table.HeaderCell>
          </Table.Row>
        </Table.Footer>
      </Table>
      </div>
    </>
  );
};

export default UsersTable;
