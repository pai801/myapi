import React from 'react';
import { Card, Grid } from 'semantic-ui-react';
import { useTranslation } from 'react-i18next';
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { renderQuota } from '../../../helpers/render';
import { AXIS_TICK, TOOLTIP_STYLE, formatCount, totalTokens } from './chartShared';

// 概览趋势图：请求 / 额度 / Token 三条独立趋势线，共用同一份扁平聚合行。
// 纯展示组件——rows 与 granularity 由 Dashboard 页面注入，组件自身不发起请求、
// 不修改入参 rows。bucket 标签直接使用后端返回的 hour/day/week 文案。
// 度量口径（total tokens）与轴/tooltip 样式复用 chartShared，与 DimensionChart 同源。

const LINE_CONFIG = {
  requests: { color: '#4318FF' },
  quota: { color: '#00B5D8' },
  tokens: { color: '#6C63FF' },
};

/**
 * 按 bucket 汇总请求 / 额度 / Token，不修改入参 rows。
 * 返回按 bucket 升序的 `{ bucket, requests, quota, tokens }` 列表。
 */
export function buildOverviewTrendData(rows) {
  const source = Array.isArray(rows) ? rows : [];
  const byBucket = new Map();

  source.forEach((row) => {
    const bucket = row?.bucket;
    if (bucket == null) return;
    if (!byBucket.has(bucket)) {
      byBucket.set(bucket, { bucket, requests: 0, quota: 0, tokens: 0 });
    }
    const point = byBucket.get(bucket);
    point.requests += Number(row?.requests) || 0;
    point.quota += Number(row?.quota) || 0;
    point.tokens += totalTokens(row);
  });

  return [...byBucket.values()].sort((a, b) =>
    String(a.bucket).localeCompare(String(b.bucket))
  );
}

function formatQuota(value) {
  return value === null || value === undefined ? value : renderQuota(value);
}

function TrendCard({ titleKey, tooltipKey, dataKey, points, formatter }) {
  const { t } = useTranslation();
  return (
    <Grid.Column>
      <Card fluid className='chart-card'>
        <Card.Content>
          <Card.Header>{t(titleKey)}</Card.Header>
          <div className='chart-container'>
            <ResponsiveContainer width='100%' height={120}>
              <LineChart data={points}>
                <CartesianGrid strokeDasharray='3 3' vertical={false} opacity={0.1} />
                <XAxis
                  dataKey='bucket'
                  axisLine={false}
                  tickLine={false}
                  tick={AXIS_TICK}
                  interval={0}
                  minTickGap={5}
                />
                <YAxis hide={true} />
                <Tooltip
                  contentStyle={TOOLTIP_STYLE}
                  formatter={(value) => [formatter(value), t(tooltipKey)]}
                  labelFormatter={(label) =>
                    `${t('dashboard.statistics.tooltip.date')}: ${label}`
                  }
                />
                <Line
                  type='monotone'
                  dataKey={dataKey}
                  stroke={LINE_CONFIG[dataKey].color}
                  strokeWidth={2}
                  dot={false}
                  activeDot={{ r: 4 }}
                />
              </LineChart>
            </ResponsiveContainer>
          </div>
        </Card.Content>
      </Card>
    </Grid.Column>
  );
}

export function TrendCharts({ rows, granularity }) {
  const points = buildOverviewTrendData(rows);

  return (
    <Grid
      columns={3}
      stackable
      className='charts-grid overview-trends'
      data-granularity={granularity}
    >
      <TrendCard
        titleKey='dashboard.charts.requests.title'
        tooltipKey='dashboard.charts.requests.tooltip'
        dataKey='requests'
        points={points}
        formatter={formatCount}
      />
      <TrendCard
        titleKey='dashboard.charts.quota.title'
        tooltipKey='dashboard.charts.quota.tooltip'
        dataKey='quota'
        points={points}
        formatter={formatQuota}
      />
      <TrendCard
        titleKey='dashboard.charts.tokens.title'
        tooltipKey='dashboard.charts.tokens.tooltip'
        dataKey='tokens'
        points={points}
        formatter={formatCount}
      />
    </Grid>
  );
}
