import React, { useEffect, useState } from 'react';
import {
  Button,
  Header,
  Icon,
  Label,
  Segment,
  Table,
} from 'semantic-ui-react';
import {
  isAdmin,
  timestamp2string,
} from '../helpers';
import { renderColorLabel } from '../helpers/render';
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
              <Table.HeaderCell width={2.4}>
                {t('log.table.time')}
              </Table.HeaderCell>
              {isAdmin() && (
                <Table.HeaderCell className='hide-on-mobile' width={0.7}>
                  {t('log.table.channel_id')}
                </Table.HeaderCell>
              )}
              {isAdmin() && (
                <Table.HeaderCell className='hide-on-mobile' width={1.5}>
                  {t('log.table.channel_name')}
                </Table.HeaderCell>
              )}
              <Table.HeaderCell width={0.8}>
                {t('log.table.type')}
              </Table.HeaderCell>
              <Table.HeaderCell width={3}>
                {t('log.table.model')}
              </Table.HeaderCell>
              <Table.HeaderCell className='hide-on-mobile' width={0.8}>
                Stream
              </Table.HeaderCell>
              {isAdmin() && (
                <Table.HeaderCell className='hide-on-mobile' width={1.2}>
                  {t('log.table.username')}
                </Table.HeaderCell>
              )}
              <Table.HeaderCell width={1.5}>
                {t('log.table.token_name')}
              </Table.HeaderCell>
              <Table.HeaderCell width={1.5}>
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
                      <Table.Cell className='hide-on-mobile'>
                        {log.channel ? (
                          <Label basic as={Link} to={`/channel/edit/${log.channel}`}>
                            {log.channel}
                          </Label>
                        ) : (
                          ''
                        )}
                      </Table.Cell>
                    )}
                    {isAdmin() && (
                      <Table.Cell className='hide-on-mobile'>{log.channel_name || ''}</Table.Cell>
                    )}
                    <Table.Cell>
                      <Label basic color='olive'>
                        {t('log.type.usage')}
                      </Label>
                    </Table.Cell>
                    <Table.Cell>
                      {log.model_name ? renderColorLabel(log.model_name) : ''}
                    </Table.Cell>
                    <Table.Cell className='hide-on-mobile'>
                      <Label basic color={log.is_stream ? 'blue' : 'grey'} size='mini'>
                        {log.is_stream ? 'true' : 'false'}
                      </Label>
                    </Table.Cell>
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
