import React, { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Dropdown,
  Form,
  Label,
  Pagination,
  Popup,
  Table,
} from 'semantic-ui-react';
import { Link } from 'react-router-dom';
import {
  API,
  copy,
  showError,
  showSuccess,
  showWarning,
  timestamp2string,
} from '../helpers';
import { renderColorLabel } from '../helpers/render';

import { ITEMS_PER_PAGE } from '../constants';

function renderTimestamp(timestamp) {
  return <>{timestamp2string(timestamp)}</>;
}

function renderStatus(status, t) {
  switch (status) {
    case 1:
      return (
        <Label basic color='green'>
          {t('token.table.status_enabled')}
        </Label>
      );
    case 2:
      return (
        <Label basic color='red'>
          {t('token.table.status_disabled')}
        </Label>
      );
    default:
      return (
        <Label basic color='black'>
          {t('token.table.status_unknown')}
        </Label>
      );
  }
}

const TokensTable = () => {
  const { t } = useTranslation();

  const COPY_OPTIONS = [
    { key: 'raw', text: t('token.copy_options.raw'), value: '' },
    { key: 'next', text: t('token.copy_options.next'), value: 'next' },
    { key: 'ama', text: t('token.copy_options.ama'), value: 'ama' },
    { key: 'opencat', text: t('token.copy_options.opencat'), value: 'opencat' },
    { key: 'lobe', text: t('token.copy_options.lobe'), value: 'lobechat' },
  ];

  const [tokens, setTokens] = useState([]);
  const [loading, setLoading] = useState(true);
  const [activePage, setActivePage] = useState(1);
  const [searchKeyword, setSearchKeyword] = useState('');
  const [searching, setSearching] = useState(false);
  const [orderBy, setOrderBy] = useState('');
  const [groupOptions, setGroupOptions] = useState([]);
  const [editingTokenId, setEditingTokenId] = useState(null);

  const fetchGroups = useCallback(async () => {
    try {
      const res = await API.get('/api/group/');
      const { success, message, data } = res.data || {};
      if (success && Array.isArray(data)) {
        setGroupOptions(
          data.map((g) => ({ key: g, text: g, value: g }))
        );
      }
    } catch (error) {
      // group list 加载失败不影响 token 列表展示
    }
  }, []);

  const loadTokens = async (startIdx) => {
    const res = await API.get(`/api/token/?p=${startIdx}&order=${orderBy}`);
    const { success, message, data } = res.data;
    if (success) {
      if (startIdx === 0) {
        setTokens(data);
      } else {
        let newTokens = [...tokens];
        newTokens.splice(startIdx * ITEMS_PER_PAGE, data.length, ...data);
        setTokens(newTokens);
      }
    } else {
      showError(message);
    }
    setLoading(false);
  };

  const onPaginationChange = (e, { activePage }) => {
    (async () => {
      if (activePage === Math.ceil(tokens.length / ITEMS_PER_PAGE) + 1) {
        await loadTokens(activePage - 1, orderBy);
      }
      setActivePage(activePage);
    })();
  };

  const refresh = async () => {
    setLoading(true);
    await loadTokens(activePage - 1);
  };

  useEffect(() => {
    loadTokens(0, orderBy)
      .then()
      .catch((reason) => {
        showError(reason);
      });
    fetchGroups().then();
  }, [orderBy, fetchGroups]);

  const onCopy = async (type, key) => {
    let status = localStorage.getItem('status');
    let serverAddress = '';
    if (status) {
      status = JSON.parse(status);
      serverAddress = status.server_address;
    }
    if (serverAddress === '') {
      serverAddress = window.location.origin;
    }
    let encodedServerAddress = encodeURIComponent(serverAddress);
    let nextUrl = `https://app.nextchat.dev/#/?settings={"key":"sk-${key}","url":"${serverAddress}"}`;

    let url;
    switch (type) {
      case 'ama':
        url = `ama://set-api-key?server=${encodedServerAddress}&key=sk-${key}`;
        break;
      case 'opencat':
        url = `opencat://team/join?domain=${encodedServerAddress}&token=sk-${key}`;
        break;
      case 'next':
        url = nextUrl;
        break;
      case 'lobechat':
        url =
          `https://app.nextchat.dev/?settings={"keyVaults":{"openai":{"apiKey":"sk-${key}","baseURL":"${serverAddress}/v1"}}}`;
        break;
      default:
        url = `sk-${key}`;
    }
    if (await copy(url)) {
      showSuccess(t('token.messages.copy_success'));
    } else {
      showWarning(t('token.messages.copy_failed'));
      setSearchKeyword(url);
    }
  };

  const manageToken = async (id, action, idx) => {
    let data = { id };
    let res;
    switch (action) {
      case 'delete':
        res = await API.delete(`/api/token/${id}/`);
        break;
      case 'enable':
        data.status = 1;
        res = await API.put('/api/token/?status_only=true', data);
        break;
      case 'disable':
        data.status = 2;
        res = await API.put('/api/token/?status_only=true', data);
        break;
    }
    const { success, message } = res.data;
    if (success) {
      showSuccess(t('token.messages.operation_success'));
      let token = res.data.data;
      let newTokens = [...tokens];
      let realIdx = (activePage - 1) * ITEMS_PER_PAGE + idx;
      if (action === 'delete') {
        newTokens[realIdx].deleted = true;
      } else {
        newTokens[realIdx].status = token.status;
      }
      setTokens(newTokens);
    } else {
      showError(message);
    }
  };

  const handleGroupChange = async (tokenId, newGroup, idx) => {
    setEditingTokenId(null);
    const res = await API.put('/api/token/?group_only=true', {
      id: tokenId,
      group: newGroup,
    });
    const { success, message } = res.data;
    if (success) {
      showSuccess(t('token.messages.operation_success'));
      let newTokens = [...tokens];
      let realIdx = (activePage - 1) * ITEMS_PER_PAGE + idx;
      newTokens[realIdx].group = newGroup;
      setTokens(newTokens);
    } else {
      showError(message);
    }
  };

  const searchTokens = async () => {
    if (searchKeyword === '') {
      await loadTokens(0);
      setActivePage(1);
      setOrderBy('');
      return;
    }
    setSearching(true);
    const res = await API.get(`/api/token/search?keyword=${searchKeyword}`);
    const { success, message, data } = res.data;
    if (success) {
      setTokens(data);
      setActivePage(1);
    } else {
      showError(message);
    }
    setSearching(false);
  };

  const handleKeywordChange = async (e, { value }) => {
    setSearchKeyword(value.trim());
  };

  const sortToken = (key) => {
    if (tokens.length === 0) return;
    setLoading(true);
    let sortedTokens = [...tokens];
    sortedTokens.sort((a, b) => {
      if (!isNaN(a[key])) {
        return a[key] - b[key];
      } else {
        return ('' + a[key]).localeCompare(b[key]);
      }
    });
    if (sortedTokens[0].id === tokens[0].id) {
      sortedTokens.reverse();
    }
    setTokens(sortedTokens);
    setLoading(false);
  };

  const handleOrderByChange = (e, { value }) => {
    setOrderBy(value);
    setActivePage(1);
  };

  return (
    <>
      <Form onSubmit={searchTokens}>
        <Form.Input
          icon='search'
          fluid
          iconPosition='left'
          placeholder={t('token.search')}
          value={searchKeyword}
          loading={searching}
          onChange={handleKeywordChange}
        />
      </Form>

      <div className='table-scroll-wrapper'>
      <Table unstackable basic={'very'} compact size='small'>
        <Table.Header>
          <Table.Row>
            <Table.HeaderCell
              style={{ cursor: 'pointer' }}
              onClick={() => sortToken('name')}
            >
              {t('token.table.name')}
            </Table.HeaderCell>
            <Table.HeaderCell
              style={{ cursor: 'pointer' }}
              onClick={() => sortToken('status')}
            >
              {t('token.table.status')}
            </Table.HeaderCell>
            <Table.HeaderCell>{t('token.table.group')}</Table.HeaderCell>
            <Table.HeaderCell
              style={{ cursor: 'pointer' }}
              onClick={() => sortToken('created_time')}
            >
              {t('token.table.created_time')}
            </Table.HeaderCell>
            <Table.HeaderCell>{t('token.table.actions')}</Table.HeaderCell>
          </Table.Row>
        </Table.Header>

        <Table.Body>
          {tokens
            .slice(
              (activePage - 1) * ITEMS_PER_PAGE,
              activePage * ITEMS_PER_PAGE
            )
            .map((token, idx) => {
              if (token.deleted) return <></>;

              const copyOptionsWithHandlers = COPY_OPTIONS.map((option) => ({
                ...option,
                onClick: async () => {
                  await onCopy(option.value, token.key);
                },
              }));

              return (
                <Table.Row key={token.id}>
                  <Table.Cell>
                    {token.name ? token.name : t('token.table.no_name')}
                  </Table.Cell>
                  <Table.Cell>{renderStatus(token.status, t)}</Table.Cell>
                  <Table.Cell>
                    {editingTokenId === token.id ? (
                      <Dropdown
                        fluid
                        selection
                        search
                        defaultOpen
                        options={groupOptions}
                        defaultValue={token.group || 'default'}
                        onClick={(e) => e.stopPropagation()}
                        onChange={(e, { value }) =>
                          handleGroupChange(token.id, value, idx)
                        }
                        onBlur={() => setEditingTokenId(null)}
                      />
                    ) : (
                      <a
                        style={{ cursor: 'pointer' }}
                        onClick={() => setEditingTokenId(token.id)}
                      >
                        {renderColorLabel(token.group || 'default')}
                      </a>
                    )}
                  </Table.Cell>
                  <Table.Cell>{renderTimestamp(token.created_time)}</Table.Cell>
                  <Table.Cell>
                    <div>
                      <Button.Group color='green' size={'tiny'}>
                        <Button
                          size={'tiny'}
                          positive
                          onClick={async () => await onCopy('', token.key)}
                        >
                          {t('token.buttons.copy')}
                        </Button>
                        <Dropdown
                          className='button icon'
                          floating
                          options={copyOptionsWithHandlers}
                          trigger={<></>}
                        />
                      </Button.Group>{' '}
                      <Popup
                        trigger={
                          <Button size='mini' negative>
                            {t('token.buttons.delete')}
                          </Button>
                        }
                        on='click'
                        flowing
                        hoverable
                      >
                        <Button
                          size={'tiny'}
                          negative
                          onClick={() => {
                            manageToken(token.id, 'delete', idx);
                          }}
                        >
                          {t('token.buttons.confirm_delete')} {token.name}
                        </Button>
                      </Popup>
                      <Button
                        size={'tiny'}
                        onClick={() => {
                          manageToken(
                            token.id,
                            token.status === 1 ? 'disable' : 'enable',
                            idx
                          );
                        }}
                      >
                        {token.status === 1
                          ? t('token.buttons.disable')
                          : t('token.buttons.enable')}
                      </Button>
                      <Button
                        size={'tiny'}
                        as={Link}
                        to={'/token/edit/' + token.id}
                      >
                        {t('token.buttons.edit')}
                      </Button>
                    </div>
                  </Table.Cell>
                </Table.Row>
              );
            })}
        </Table.Body>

      </Table>
      </div>
      <div className='table-footer-toolbar scroll-x-nowrap'>
        <Button size='small' as={Link} to='/token/add' loading={loading}>
          {t('token.buttons.add')}
        </Button>
        <Button size='small' onClick={refresh} loading={loading}>
          {t('token.buttons.refresh')}
        </Button>
        <Dropdown
          placeholder={t('token.sort.placeholder')}
          selection
          options={[
            { key: '', text: t('token.sort.default'), value: '' },
          ]}
          value={orderBy}
          onChange={handleOrderByChange}
          style={{ marginLeft: '10px' }}
        />
        <Pagination
          className='table-footer-pagination'
          floated='right'
          activePage={activePage}
          onPageChange={onPaginationChange}
          size='small'
          siblingRange={1}
          totalPages={
            Math.ceil(tokens.length / ITEMS_PER_PAGE) +
            (tokens.length % ITEMS_PER_PAGE === 0 ? 1 : 0)
          }
        />
      </div>
    </>
  );
};

export default TokensTable;
