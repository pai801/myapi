import React, { useEffect, useRef, useState } from 'react';
import {
  Button,
  Form,
  Header,
  Label,
  Pagination,
  Segment,
  Select,
  Table,
  Popup,
} from 'semantic-ui-react';
import {
  API,
  copy,
  isAdmin,
  showError,
  showSuccess,
  showWarning,
  timestamp2string,
} from '../helpers';
import { useTranslation } from 'react-i18next';

import { ITEMS_PER_PAGE } from '../constants';
import { renderColorLabel, renderQuota } from '../helpers/render';
import { Link } from 'react-router-dom';
import DetailDialog from './DetailDialog';
import ActiveRequestsPanel from './ActiveRequestsPanel';

function renderTimestamp(timestamp, request_id) {
  return (
    <code
      onClick={async () => {
        if (await copy(request_id)) {
          showSuccess(`已复制请求 ID：${request_id}`);
        } else {
          showWarning(`请求 ID 复制失败：${request_id}`);
        }
      }}
      style={{ cursor: 'pointer' }}
    >
      {timestamp2string(timestamp)}
    </code>
  );
}

const MODE_OPTIONS = [
  { key: 'all', text: '全部用户', value: 'all' },
  { key: 'self', text: '当前用户', value: 'self' },
];

function renderType(type, t) {
  switch (type) {
    case 1:
      return (
        <Label basic color='green'>
          {t('log.type.topup')}
        </Label>
      );
    case 2:
      return (
        <Label basic color='olive'>
          {t('log.type.usage')}
        </Label>
      );
    case 3:
      return (
        <Label basic color='orange'>
          {t('log.type.admin')}
        </Label>
      );
    case 4:
      return (
        <Label basic color='purple'>
          {t('log.type.system')}
        </Label>
      );
    case 5:
      return (
        <Label basic color='violet'>
          {t('log.type.test')}
        </Label>
      );
    default:
      return (
        <Label basic color='black'>
          未知
        </Label>
      );
  }
}

function getColorByElapsedTime(elapsedTime) {
  if (elapsedTime === undefined || 0) return 'black';
  if (elapsedTime < 1000) return 'green';
  if (elapsedTime < 3000) return 'olive';
  if (elapsedTime < 5000) return 'yellow';
  if (elapsedTime < 10000) return 'orange';
  return 'red';
}

function renderDetail(log) {
  return (
    <>
      {log.content}
      <br />
      {log.elapsed_time && (
        <Label
          basic
          size={'mini'}
          color={getColorByElapsedTime(log.elapsed_time)}
        >
          {log.elapsed_time} ms
        </Label>
      )}
      {log.is_stream && (
        <>
          <Label size={'mini'} color='pink'>
            Stream
          </Label>
        </>
      )}
      {log.system_prompt_reset && (
        <>
          <Label basic size={'mini'} color='red'>
            System Prompt Reset
          </Label>
        </>
      )}
    </>
  );
}

const LogsTable = () => {
  const { t } = useTranslation();
  const [logs, setLogs] = useState([]);
  const [pageData, setPageData] = useState([]);
  const [showStat, setShowStat] = useState(false);
  const [loading, setLoading] = useState(true);
  const [activePage, setActivePage] = useState(1);
  const activePageRef = useRef(1);
  // 同步 activePage 到 ref，供 SSE 事件处理器读取最新值
  useEffect(() => { activePageRef.current = activePage; }, [activePage]);
  const [totalLogs, setTotalLogs] = useState(0);
  const [jumpPage, setJumpPage] = useState('');
  const [searchKeyword, setSearchKeyword] = useState('');
  const [searching, setSearching] = useState(false);
  const [logType, setLogType] = useState(0);
  const isAdminUser = isAdmin();
  const [selectedLogItem, setSelectedLogItem] = useState(null);
  const [detailDialogOpen, setDetailDialogOpen] = useState(false);
  const [detailLoading, setDetailLoading] = useState(false);
  const [activeLogs, setActiveLogs] = useState([]);
  const [selectedActiveLog, setSelectedActiveLog] = useState(null);
  const [activeDetailOpen, setActiveDetailOpen] = useState(false);

  const handleDetailClick = async (logItem) => {
    if (detailLoading) return;
    setDetailLoading(true);
    try {
      const res = await API.get(`/api/log/${logItem.id}`);
      const { success, message, data } = res.data;
      if (success) {
        setSelectedLogItem(data);
        setDetailDialogOpen(true);
      } else {
        showError(message);
      }
    } catch (err) {
      showError(t('log.messages.detail_failed'));
    } finally {
      setDetailLoading(false);
    }
  };

  const handleDetailClose = () => {
    setDetailDialogOpen(false);
  };

  const handleActiveDetailClick = (log) => {
    setSelectedActiveLog(log);
    setActiveDetailOpen(true);
  };

  const handleActiveDetailClose = () => {
    setActiveDetailOpen(false);
  };

  const [inputs, setInputs] = useState({
    username: '',
    token_name: '',
    model_name: '',
    start_timestamp: timestamp2string(0),
    end_timestamp: timestamp2string(0),
    channel: '',
  });
  const {
    username,
    token_name,
    model_name,
    start_timestamp,
    end_timestamp,
    channel,
  } = inputs;

  const [stat, setStat] = useState({
    quota: 0,
    token: 0,
  });

  const LOG_OPTIONS = [
    { key: '0', text: t('log.type.all'), value: 0 },
    { key: '1', text: t('log.type.topup'), value: 1 },
    { key: '2', text: t('log.type.usage'), value: 2 },
    { key: '3', text: t('log.type.admin'), value: 3 },
    { key: '4', text: t('log.type.system'), value: 4 },
    { key: '5', text: t('log.type.test'), value: 5 },
  ];

  const handleInputChange = (e, { name, value }) => {
    setInputs((inputs) => ({ ...inputs, [name]: value }));
  };

  const getLogSelfStat = async () => {
    let localStartTimestamp = Date.parse(start_timestamp) / 1000;
    let localEndTimestamp = Date.parse(end_timestamp) / 1000;
    let res = await API.get(
      `/api/log/self/stat?type=${logType}&token_name=${token_name}&model_name=${model_name}&start_timestamp=${localStartTimestamp}&end_timestamp=${localEndTimestamp}`
    );
    const { success, message, data } = res.data;
    if (success) {
      setStat(data);
    } else {
      showError(message);
    }
  };

  const getLogStat = async () => {
    let localStartTimestamp = Date.parse(start_timestamp) / 1000;
    let localEndTimestamp = Date.parse(end_timestamp) / 1000;
    let res = await API.get(
      `/api/log/stat?type=${logType}&username=${username}&token_name=${token_name}&model_name=${model_name}&start_timestamp=${localStartTimestamp}&end_timestamp=${localEndTimestamp}&channel=${channel}`
    );
    const { success, message, data } = res.data;
    if (success) {
      setStat(data);
    } else {
      showError(message);
    }
  };

  const handleEyeClick = async () => {
    if (!showStat) {
      if (isAdminUser) {
        await getLogStat();
      } else {
        await getLogSelfStat();
      }
    }
    setShowStat(!showStat);
  };

  const showUserTokenQuota = () => {
    return logType !== 5;
  };

  const loadLogs = async (startIdx) => {
    let url = '';
    let localStartTimestamp = Date.parse(start_timestamp) / 1000;
    let localEndTimestamp = Date.parse(end_timestamp) / 1000;
    if (isAdminUser) {
      url = `/api/log/?p=${startIdx}&type=${logType}&username=${username}&token_name=${token_name}&model_name=${model_name}&start_timestamp=${localStartTimestamp}&end_timestamp=${localEndTimestamp}&channel=${channel}`;
    } else {
      url = `/api/log/self/?p=${startIdx}&type=${logType}&token_name=${token_name}&model_name=${model_name}&start_timestamp=${localStartTimestamp}&end_timestamp=${localEndTimestamp}`;
    }
    const res = await API.get(url);
    const { success, message, data, total } = res.data;
    if (success) {
      if (startIdx === 0) {
        setLogs(data);
      }
      setPageData(data || []);
      if (total !== undefined) {
        setTotalLogs(total);
      }
    } else {
      showError(message);
    }
    setLoading(false);
  };

  const onPaginationChange = (e, { activePage }) => {
    setActivePage(activePage);
    setLoading(true);
    loadLogs(activePage - 1);
  };

  const handleJumpPage = () => {
    const pageNum = parseInt(jumpPage, 10);
    const total = Math.max(1, Math.ceil(totalLogs / ITEMS_PER_PAGE));
    if (isNaN(pageNum) || pageNum < 1 || pageNum > total) {
      showError(`请输入 1-${total} 之间的页码`);
      return;
    }
    setJumpPage('');
    setActivePage(pageNum);
    setLoading(true);
    loadLogs(pageNum - 1);
  };

  const refresh = async () => {
    setLoading(true);
    setActivePage(1);
    setTotalLogs(0);
    setPageData([]);
    await loadLogs(0);
  };

  useEffect(() => {
    refresh().then();
  }, [logType]);

  // SSE 实时推送活跃请求（仅管理员）
  useEffect(() => {
    if (!isAdminUser) return;

    const baseUrl = process.env.REACT_APP_SERVER || '';
    const es = new EventSource(`${baseUrl}/api/log/active/events`, { withCredentials: true });

    es.addEventListener('start', (e) => {
      const evt = JSON.parse(e.data);
      setActiveLogs(prev => {
        // upsert: 快照 + start 事件竞态导致同一 request_id 可能出现两次
        if (prev.some(r => r.request_id === evt.data.request_id)) return prev;
        return [...prev, evt.data];
      });
    });

    es.addEventListener('update', (e) => {
      const evt = JSON.parse(e.data);
      setActiveLogs(prev => prev.map(r =>
        r.request_id === evt.data.request_id ? evt.data : r
      ));
    });

    es.addEventListener('end', (e) => {
      const evt = JSON.parse(e.data);
      setActiveLogs(prev => prev.filter(r => r.request_id !== evt.request_id));
    });

    es.addEventListener('complete', (e) => {
      const evt = JSON.parse(e.data);
      const log = evt.log;
      if (!log || activePageRef.current !== 1) return;
      // 完整 DB 日志，仅补充 type 字段（固定为消费日志）
      const newLog = { ...log, type: 2 };
      setPageData(prev => {
        const next = [newLog, ...prev];
        if (next.length > ITEMS_PER_PAGE) next.pop();
        return next;
      });
      setTotalLogs(prev => prev + 1);
    });

    return () => es.close();
  }, [isAdminUser]);

  const searchLogs = async () => {
    if (searchKeyword === '') {
      // if keyword is blank, load files instead.
      setActivePage(1);
      setLoading(true);
      loadLogs(0);
      return;
    }
    setSearching(true);
    const res = await API.get(`/api/log/self/search?keyword=${searchKeyword}`);
    const { success, message, data } = res.data;
    if (success) {
      setLogs(data);
      setPageData(data);
      setTotalLogs(data.length);
      setActivePage(1);
    } else {
      showError(message);
    }
    setSearching(false);
  };

  const handleKeywordChange = async (e, { value }) => {
    setSearchKeyword(value.trim());
  };

  const sortLog = (key) => {
    if (pageData.length === 0) return;
    setLoading(true);
    let sortedLogs = [...pageData];
    if (typeof sortedLogs[0][key] === 'string') {
      sortedLogs.sort((a, b) => {
        return ('' + a[key]).localeCompare(b[key]);
      });
    } else {
      sortedLogs.sort((a, b) => {
        if (a[key] === b[key]) return 0;
        if (a[key] > b[key]) return -1;
        if (a[key] < b[key]) return 1;
      });
    }
    if (sortedLogs[0].id === pageData[0].id) {
      sortedLogs.reverse();
    }
    setPageData(sortedLogs);
    setLoading(false);
  };

  return (
    <>
      <DetailDialog open={detailDialogOpen} onClose={handleDetailClose} logItem={selectedLogItem} />
      <ActiveRequestsPanel
        logs={activeLogs}
        onDetailClick={handleActiveDetailClick}
      />
      <DetailDialog
        open={activeDetailOpen}
        onClose={handleActiveDetailClose}
        logItem={selectedActiveLog}
        isActive={true}
      />
      <Header as='h3'>
        {t('log.usage_details')}（{t('log.total_quota')}：
        {showStat && renderQuota(stat.quota, t)}
        {!showStat && (
          <span
            onClick={handleEyeClick}
            style={{ cursor: 'pointer', color: 'gray' }}
          >
            {t('log.click_to_view')}
          </span>
        )}
        ）
      </Header>
      <Form>
        <div className='scroll-x-nowrap'>
          <Form.Group unstackable>
            <Form.Input
              fluid
              label={t('log.table.token_name')}
              size={'small'}
              width={3}
              value={token_name}
              placeholder={t('log.table.token_name_placeholder')}
              name='token_name'
              onChange={handleInputChange}
            />
            <Form.Input
              fluid
              label={t('log.table.model_name')}
              size={'small'}
              width={3}
              value={model_name}
              placeholder={t('log.table.model_name_placeholder')}
              name='model_name'
              onChange={handleInputChange}
            />
            <Form.Input
              fluid
              label={t('log.table.start_time')}
              size={'small'}
              width={4}
              value={start_timestamp}
              type='datetime-local'
              name='start_timestamp'
              onChange={handleInputChange}
            />
            <Form.Input
              fluid
              label={t('log.table.end_time')}
              size={'small'}
              width={4}
              value={end_timestamp}
              type='datetime-local'
              name='end_timestamp'
              onChange={handleInputChange}
            />
            <Form.Button
              fluid
              label={t('log.buttons.query')}
              size={'small'}
              width={2}
              onClick={refresh}
            >
              {t('log.buttons.submit')}
            </Form.Button>
          </Form.Group>
        </div>
        {isAdminUser && (
          <>
            <div className='scroll-x-nowrap'>
              <Form.Group unstackable>
                <Form.Input
                  fluid
                  label={t('log.table.channel_id')}
                  size={'small'}
                  width={3}
                  value={channel}
                  placeholder={t('log.table.channel_id_placeholder')}
                  name='channel'
                  onChange={handleInputChange}
                />
                <Form.Input
                  fluid
                  label={t('log.table.username')}
                  size={'small'}
                  width={3}
                  value={username}
                  placeholder={t('log.table.username_placeholder')}
                  name='username'
                  onChange={handleInputChange}
                />
              </Form.Group>
            </div>
          </>
        )}
        <Form.Input
          icon='search'
          placeholder={t('log.search')}
          value={searchKeyword}
          onChange={(e, { value }) => setSearchKeyword(value)}
        />
      </Form>
      <div className='table-scroll-wrapper'>
      <Table unstackable basic={'very'} compact size='small'>
        <Table.Header>
          <Table.Row>
            <Table.HeaderCell
              style={{ cursor: 'pointer' }}
              onClick={() => {
                sortLog('created_time');
              }}
              width={2.4}
            >
              {t('log.table.time')}
            </Table.HeaderCell>
            {isAdminUser && (
              <Table.HeaderCell
                className='hide-on-mobile'
                style={{ cursor: 'pointer' }}
                onClick={() => {
                  sortLog('channel');
                }}
                width={0.7}
              >
                {t('log.table.channel_id')}
              </Table.HeaderCell>
            )}
            {isAdminUser && (
              <Table.HeaderCell
                className='hide-on-mobile'
                style={{ cursor: 'pointer' }}
                onClick={() => {
                  sortLog('channel_name');
                }}
                width={1.5}
              >
                {t('log.table.channel_name')}
              </Table.HeaderCell>
            )}
            <Table.HeaderCell
              style={{ cursor: 'pointer' }}
              onClick={() => {
                sortLog('type');
              }}
              width={0.8}
            >
              {t('log.table.type')}
            </Table.HeaderCell>
            <Table.HeaderCell
              style={{ cursor: 'pointer' }}
              onClick={() => {
                sortLog('model_name');
              }}
              width={3}
            >
              {t('log.table.model')}
            </Table.HeaderCell>
            <Table.HeaderCell
              className='hide-on-mobile'
              style={{ cursor: 'pointer' }}
              onClick={() => {
                sortLog('is_stream');
              }}
              width={0.8}
            >
              Stream
            </Table.HeaderCell>
            {showUserTokenQuota() && (
              <>
                {isAdminUser && (
                  <Table.HeaderCell
                    className='hide-on-mobile'
                    style={{ cursor: 'pointer' }}
                    onClick={() => {
                      sortLog('username');
                    }}
                    width={1.2}
                  >
                    {t('log.table.username')}
                  </Table.HeaderCell>
                )}
                <Table.HeaderCell
                  style={{ cursor: 'pointer' }}
                  onClick={() => {
                    sortLog('token_name');
                  }}
                  width={1.5}
                >
                  {t('log.table.token_name')}
                </Table.HeaderCell>
                <Table.HeaderCell
                  className='hide-on-mobile'
                  style={{ cursor: 'pointer' }}
                  onClick={() => {
                    sortLog('prompt_tokens');
                  }}
                  width={1}
                >
                  {t('log.table.prompt_tokens')}
                </Table.HeaderCell>
                <Table.HeaderCell
                  className='hide-on-mobile'
                  style={{ cursor: 'pointer' }}
                  onClick={() => {
                    sortLog('cached_tokens');
                  }}
                  width={1.5}
                >
                  {t('log.table.cached_tokens')}
                </Table.HeaderCell>
                <Table.HeaderCell
                  className='hide-on-mobile'
                  style={{ cursor: 'pointer' }}
                  onClick={() => {
                    sortLog('completion_tokens');
                  }}
                  width={1}
                >
                  {t('log.table.completion_tokens')}
                </Table.HeaderCell>
                <Table.HeaderCell
                  style={{ cursor: 'pointer' }}
                  onClick={() => {
                    sortLog('quota');
                  }}
                  width={1}
                >
                  {t('log.table.quota')}
                </Table.HeaderCell>
              </>
            )}
            {isAdminUser && <Table.HeaderCell>{t('log.table.detail')}</Table.HeaderCell>}
          </Table.Row>
        </Table.Header>

        <Table.Body>
          {pageData
            .filter(log => log != null)
            .map((log, idx) => {
              if (log.deleted) return <></>;
              const allTokensZero = (!log.prompt_tokens || log.prompt_tokens === 0) &&
                (!log.completion_tokens || log.completion_tokens === 0) &&
                (!log.quota || log.quota === 0);
              return (
                <Table.Row key={log.id}>
                  <Table.Cell>
                    {renderTimestamp(log.created_at, log.request_id)}
                  </Table.Cell>
                  {isAdminUser && (
                    <Table.Cell className='hide-on-mobile'>
                      {log.channel ? (
                        <Label
                          basic
                          as={Link}
                          to={`/channel/edit/${log.channel}`}
                        >
                          {log.channel}
                        </Label>
                      ) : (
                        ''
                      )}
                    </Table.Cell>
                  )}
                  {isAdminUser && (
                    <Table.Cell className='hide-on-mobile'>
                      {log.channel_name || ''}
                    </Table.Cell>
                  )}
                  <Table.Cell>{renderType(log.type, t)}</Table.Cell>
                  <Table.Cell>
                    {log.model_name ? renderColorLabel(log.model_name) : ''}
                  </Table.Cell>
                  <Table.Cell className='hide-on-mobile'>
                    <Label basic color={log.is_stream ? 'blue' : 'grey'} size='mini'>
                      {log.is_stream ? 'true' : 'false'}
                    </Label>
                  </Table.Cell>
                  {showUserTokenQuota() && (
                    <>
                      {isAdminUser && (
                        <Table.Cell className='hide-on-mobile'>
                          {log.username ? (
                            <Label
                              basic
                              as={Link}
                              to={`/user/edit/${log.user_id}`}
                            >
                              {log.username}
                            </Label>
                          ) : (
                            ''
                          )}
                        </Table.Cell>
                      )}
                      <Table.Cell>
                        {log.token_name ? renderColorLabel(log.token_name) : ''}
                      </Table.Cell>

                      <Table.Cell className='hide-on-mobile'>
                        {allTokensZero ? '-' : (log.prompt_tokens || 0)}
                      </Table.Cell>
                      <Table.Cell className='hide-on-mobile'>
                        {log.cached_tokens ? (log.prompt_tokens && log.prompt_tokens > 0 ? `${log.cached_tokens} (${(log.cached_tokens / log.prompt_tokens * 100).toFixed(2)}%)` : log.cached_tokens) : ''}
                      </Table.Cell>
                      <Table.Cell className='hide-on-mobile'>
                        {allTokensZero ? '-' : (log.completion_tokens || 0)}
                      </Table.Cell>
                      <Table.Cell>
                        {allTokensZero ? '-' : (log.quota ? renderQuota(log.quota, t, 6) : '0')}
                      </Table.Cell>
                    </>
                  )}

                  {isAdminUser ? (
                    <Table.Cell>
                      <Button size='mini' onClick={() => handleDetailClick(log)} disabled={!log.has_request_body && !log.has_response_body && !log.has_request_header} loading={detailLoading}>{t('log.table.detail')}</Button>
                    </Table.Cell>
                  ) : (
                    <Table.Cell>{renderDetail(log)}</Table.Cell>
                  )}
                </Table.Row>
              );
            })}
        </Table.Body>

      </Table>
      </div>
      <div className='table-footer-toolbar scroll-x-nowrap'>
        <Select
          placeholder={t('log.type.select')}
          options={LOG_OPTIONS}
          style={{ marginRight: '8px' }}
          name='logType'
          value={logType}
          onChange={(e, { name, value }) => {
            setLogType(value);
          }}
        />
        <Button size='small' onClick={refresh} loading={loading}>
          {t('log.buttons.refresh')}
        </Button>
        <span className='hide-on-mobile' style={{ marginRight: '8px', fontSize: '13px', display: 'inline-flex', alignItems: 'center', gap: '4px' }}>
          {t('pagination.jump_to')}
          <input
            type='text'
            value={jumpPage}
            onChange={(e) => {
              const val = e.target.value;
              if (val === '' || /^\d+$/.test(val)) {
                setJumpPage(val);
              }
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter') handleJumpPage();
            }}
            placeholder={t('pagination.page_placeholder')}
            style={{
              width: '48px',
              padding: '4px 6px',
              border: '1px solid rgba(34,36,38,.15)',
              borderRadius: '4px',
              textAlign: 'center',
              fontSize: '13px',
            }}
          />
          {t('pagination.page')}
          <Button
            size='mini'
            onClick={handleJumpPage}
            disabled={loading}
          >
            GO
          </Button>
        </span>
        <Pagination
          className='table-footer-pagination'
          floated='right'
          activePage={activePage}
          onPageChange={onPaginationChange}
          size='small'
          siblingRange={1}
          firstItem={{ content: t('pagination.first_page'), 'aria-label': t('pagination.first_page_aria') }}
          lastItem={{ content: t('pagination.last_page'), 'aria-label': t('pagination.last_page_aria') }}
          totalPages={
            Math.max(1, Math.ceil(totalLogs / ITEMS_PER_PAGE))
          }
        />
      </div>
    </>
  );
};

export default LogsTable;
