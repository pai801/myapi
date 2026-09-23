import React from 'react';
import { Card, Grid } from 'semantic-ui-react';
import { useTranslation } from 'react-i18next';
import { renderQuota } from '../../../helpers/render';

/**
 * @typedef {Object} DashboardSummary
 * @property {number} total_requests
 * @property {number} total_quota
 * @property {number} total_tokens
 * @property {number} cached_tokens
 * @property {number} cache_hit_rate
 * @property {number} avg_ttft
 * @property {number} avg_elapsed
 * @property {number} avg_rpm
 * @property {number} avg_tpm
 */

// KPI 卡片条：纯展示组件，数据与状态由 Dashboard 页面通过 props 注入，
// 组件自身不发起任何请求、不持有跨渲染状态。

/** 无样本 / 非有限值的统一占位符。 */
const EMPTY_MARKER = '--';

// 仅有限数字可展示；0 是有效值，undefined/null/NaN/Infinity 一律视为不可用样本。
function isDisplayable(value) {
  return typeof value === 'number' && Number.isFinite(value);
}

function formatCount(value) {
  return isDisplayable(value)
    ? Math.round(value).toLocaleString('en-US')
    : EMPTY_MARKER;
}

// 额度沿用应用既有展示约定（renderQuota：k/M/B 缩写）。
function formatQuota(value) {
  return isDisplayable(value) ? renderQuota(value) : EMPTY_MARKER;
}

function formatRate(value) {
  return isDisplayable(value) ? `${(value * 100).toFixed(2)}%` : EMPTY_MARKER;
}

function formatMilliseconds(value) {
  return isDisplayable(value) ? `${value.toFixed(1)} ms` : EMPTY_MARKER;
}

function formatDecimal(value) {
  return isDisplayable(value) ? value.toFixed(2) : EMPTY_MARKER;
}

export function SummaryCards({ summary }) {
  const { t } = useTranslation();
  const source = summary || {};

  const metrics = [
    { key: 'total_requests', format: formatCount },
    { key: 'total_quota', format: formatQuota },
    { key: 'total_tokens', format: formatCount },
    { key: 'cached_tokens', format: formatCount },
    { key: 'cache_hit_rate', format: formatRate },
    { key: 'avg_ttft', format: formatMilliseconds },
    { key: 'avg_elapsed', format: formatMilliseconds },
    { key: 'avg_rpm', format: formatDecimal },
    { key: 'avg_tpm', format: formatDecimal },
  ];

  return (
    <Grid columns={5} stackable className='summary-cards'>
      {metrics.map(({ key, format }) => (
        <Grid.Column key={key}>
          <Card fluid className='summary-card'>
            <Card.Content>
              <div className='summary-card-label'>
                {t(`dashboard.summary.${key}`)}
              </div>
              <div className='summary-card-value'>{format(source[key])}</div>
            </Card.Content>
          </Card>
        </Grid.Column>
      ))}
    </Grid>
  );
}
