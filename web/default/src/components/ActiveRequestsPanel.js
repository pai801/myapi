import React, { useEffect, useMemo, useState } from 'react';
import {
  Button,
  Header,
  Icon,
  Label,
  Popup,
  Segment,
  Table,
} from 'semantic-ui-react';
import {
  isAdmin,
  timestamp2string,
} from '../helpers';
import { renderColorLabel } from '../helpers/render';
import { extractUserMessage } from '../helpers/liveRequestMessage';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';

function getColorByElapsedTime(elapsedTime) {
  if (elapsedTime === undefined || elapsedTime === 0) return 'black';
  if (elapsedTime < 1000) return 'green';
  if (elapsedTime < 3000) return 'olive';
  if (elapsedTime < 5000) return 'yellow';
  if (elapsedTime < 10000) return 'orange';
  return 'red';
}

const MESSAGE_MAX_LENGTH = 36;

// 单元格内单行展示，超长截断；无论是否截断，hover 都浮出完整原文
function renderMessage(text) {
  if (!text) return '-';
  const oneLine = text.replace(/\s+/g, ' ').trim();
  const display =
    oneLine.length <= MESSAGE_MAX_LENGTH
      ? oneLine
      : `${oneLine.slice(0, MESSAGE_MAX_LENGTH)}…`;
  return (
    <Popup
      content={
        <div
          style={{
            whiteSpace: 'pre-wrap',
            maxWidth: '520px',
            maxHeight: '320px',
            overflow: 'auto',
          }}
        >
          {text}
        </div>
      }
      trigger={
        <span style={{ display: 'block', width: '100%', cursor: 'default' }}>{display}</span>
      }
      basic
      hoverable
      wide='very'
    />
  );
}

const ActiveRequestsPanel = ({ logs, onDetailClick }) => {
  const { t } = useTranslation();
  const [collapsed, setCollapsed] = useState(false);
  const hasLogs = logs && logs.length > 0;
  // 定时触发重渲染，使 Elapsed 列自动增长。
  // 计时器不依赖 logs 内容变化，始终以 200ms 周期运行。
  const [, setTick] = useState(0);

  useEffect(() => {
    if (!hasLogs) return;
    const timer = setInterval(() => setTick(t => t + 1), 200);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hasLogs]);

  // 请求体只在 logs 引用变化时解析一次，避免 200ms 计时器每 tick 重复 JSON.parse
  const messageMap = useMemo(() => {
    const map = {};
    (logs || []).forEach((log) => {
      map[log.request_id] = extractUserMessage(log.request_body);
    });
    return map;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [logs]);

  return (
    <Segment color={hasLogs ? 'red' : 'grey'} style={{ marginBottom: '1em' }}>
      <Header
        as='h5'
        style={{ cursor: 'pointer', userSelect: 'none' }}
        onClick={() => setCollapsed(!collapsed)}
      >
        Live Requests ({hasLogs ? logs.length : 0})
        <Icon name={collapsed ? 'chevron down' : 'chevron up'} fitted />
      </Header>
      {!collapsed && (
        <div className='table-scroll-wrapper'>
        <Table unstackable compact basic='very' size='small'>
          <Table.Header>
            <Table.Row>
              <Table.HeaderCell width={2}>
                {t('log.table.time')}
              </Table.HeaderCell>
              {isAdmin() && (
                <Table.HeaderCell className='hide-on-mobile' width={1}>
                  {t('log.table.channel_name')}
                </Table.HeaderCell>
              )}
              <Table.HeaderCell width={2}>
                {t('log.table.model')}
              </Table.HeaderCell>
              <Table.HeaderCell width={6}>
                消息
              </Table.HeaderCell>
              {isAdmin() && (
                <Table.HeaderCell className='hide-on-mobile' width={1}>
                  {t('log.table.username')}
                </Table.HeaderCell>
              )}
              <Table.HeaderCell width={1}>
                {t('log.table.token_name')}
              </Table.HeaderCell>
              <Table.HeaderCell width={1}>
                {t('log.table.first_token_time')}
              </Table.HeaderCell>
              <Table.HeaderCell width={1}>
                Elapsed
              </Table.HeaderCell>
              <Table.HeaderCell width={0.8}>
                {t('log.table.detail')}
              </Table.HeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {hasLogs ? (
              logs.map((log) => {
                const elapsed = Date.now() - log.started_at;
                return (
                  <Table.Row key={log.request_id}>
                    <Table.Cell>
                      {timestamp2string(log.started_at / 1000)}
                    </Table.Cell>
                    {isAdmin() && (
                      <Table.Cell className='hide-on-mobile'>{log.channel_name || ''}</Table.Cell>
                    )}
                    <Table.Cell>
                      {log.model_name ? renderColorLabel(log.model_name.toLowerCase()) : ''}
                    </Table.Cell>
                    <Table.Cell>{renderMessage(messageMap[log.request_id])}</Table.Cell>
                    {isAdmin() && (
                      <Table.Cell className='hide-on-mobile'>
                        {log.username ? (
                          <Label basic as={Link} to={`/user/edit/${log.user_id}`}>
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
                    <Table.Cell>
                      {log.first_token_ms ? (
                        <Label basic size='mini' color='teal'>
                          {log.first_token_ms} ms
                        </Label>
                      ) : (
                        '-'
                      )}
                    </Table.Cell>
                    <Table.Cell>
                      <Label basic size='mini' color={getColorByElapsedTime(elapsed)}>
                        {elapsed} ms
                      </Label>
                    </Table.Cell>
                    <Table.Cell>
                      <Button
                        size='mini'
                        onClick={() => onDetailClick(log)}
                        disabled={!log.has_request_body && !log.has_request_header}
                      >
                        {t('log.table.detail')}
                      </Button>
                    </Table.Cell>
                  </Table.Row>
                );
              })
            ) : (
              <Table.Row>
                <Table.Cell colSpan='99' style={{ textAlign: 'center', color: '#999' }}>
                  暂无活跃请求
                </Table.Cell>
              </Table.Row>
            )}
          </Table.Body>
        </Table>
        </div>
      )}
    </Segment>
  );
};

export default ActiveRequestsPanel;
