import React from 'react';
import { Button, Card, Dropdown } from 'semantic-ui-react';
import { useTranslation } from 'react-i18next';
import {
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Legend,
  Line,
  LineChart,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { renderQuota } from '../../../helpers/render';
import { AXIS_TICK, TOOLTIP_STYLE, formatCount, totalTokens } from './chartShared';

// 单维度分析图：趋势折线 / 占比饼图 / Top-N 横向柱状图三视图共用同一份聚合行。
// 纯展示组件——rows/view/topN 由 Dashboard 页面注入，视图切换只回调页面，
// 组件自身不发起请求、不修改入参 rows。
//
// 三视图的通用度量是 total tokens（prompt_tokens + completion_tokens）；
// rows 同时提供给 tooltip，用于补充每次聚合的 request 与 quota 上下文。

/**
 * @typedef {'model'|'channel'|'token'|'user'} DashboardDimension
 * @typedef {'trend'|'proportion'|'top'} DashboardChartView
 * @typedef {5|10|20|50} DashboardTopN
 * @typedef {Object} DashboardAggregateRow
 * @property {string} bucket
 * @property {string} key
 * @property {number} requests
 * @property {number} quota
 * @property {number} prompt_tokens
 * @property {number} completion_tokens
 * @property {number} cached_tokens
 * @property {number} avg_ttft
 * @property {number} avg_elapsed
 */

/** Top-N 闭集，默认 10（与契约一致）。 */
const TOP_N_OPTIONS = [5, 10, 20, 50];
const DEFAULT_TOP_N = 10;

// 与既有 Dashboard 视觉保持一致的一组分类色。
const PALETTE = [
  '#4318FF',
  '#00B5D8',
  '#6C63FF',
  '#05CD99',
  '#FFB547',
  '#FF5E7D',
  '#41B883',
  '#7983FF',
  '#FF8F6B',
  '#49BEFF',
];

// 趋势点上的序列真值以 Symbol 键的 Map 存放：维度键可能叫 `bucket` 或 `__proto__`，
// 直接作为对象属性会与 X 轴字段撞名、或触发原型 setter 导致序列丢失/标签被覆盖。
// 该 Symbol 一并导出，供测试直接校验被保留的序列真值。
export const DIMENSION_SERIES_VALUES = Symbol('dimensionSeriesValues');

function colorAt(index) {
  return PALETTE[index % PALETTE.length];
}

/** 从聚合行汇总每个维度键的请求/额度/token，供 tooltip 展示 request/quota 上下文。 */
function buildKeyContext(rows) {
  const map = new Map();
  (Array.isArray(rows) ? rows : []).forEach((row) => {
    const key = row?.key;
    if (key == null) return;
    const entry = map.get(key) || { requests: 0, quota: 0, tokens: 0 };
    entry.requests += Number(row?.requests) || 0;
    entry.quota += Number(row?.quota) || 0;
    entry.tokens += totalTokens(row);
    map.set(key, entry);
  });
  return map;
}

/** 从聚合行汇总每个 (bucket, key) 的请求/额度/token，供趋势 tooltip 使用。 */
function buildBucketKeyContext(rows) {
  const map = new Map();
  (Array.isArray(rows) ? rows : []).forEach((row) => {
    const bucket = row?.bucket;
    const key = row?.key;
    if (bucket == null || key == null) return;
    if (!map.has(bucket)) map.set(bucket, new Map());
    const perBucket = map.get(bucket);
    const entry = perBucket.get(key) || { requests: 0, quota: 0, tokens: 0 };
    entry.requests += Number(row?.requests) || 0;
    entry.quota += Number(row?.quota) || 0;
    entry.tokens += totalTokens(row);
    perBucket.set(key, entry);
  });
  return map;
}

/**
 * 构建趋势视图的 bucket/key 序列，不修改入参 rows。
 * 返回 { keys, points }：keys 为按字典序排列的维度键，points 为按 bucket 升序的
 * 记录（缺失组合补 0）。每个 point 的 `bucket` 为 X 轴标签，序列真值仅以 Symbol Map
 * 存放，避免与 `bucket`/`__proto__` 等键名撞车；不再在 point 上暴露同名普通属性。
 */
export function buildDimensionTrendData(rows) {
  const source = Array.isArray(rows) ? rows : [];
  const keys = [
    ...new Set(source.map((row) => row?.key).filter((key) => key != null)),
  ].sort();
  const byBucket = new Map();

  source.forEach((row) => {
    const bucket = row?.bucket;
    if (bucket == null) return;
    if (!byBucket.has(bucket)) {
      // point 仅承载 X 轴标签 `bucket` 与 Symbol 序列 Map；序列真值只经 Map 暴露。
      const point = Object.create(null);
      point.bucket = bucket;
      point[DIMENSION_SERIES_VALUES] = new Map(keys.map((key) => [key, 0]));
      byBucket.set(bucket, point);
    }
    const key = row?.key;
    if (key == null) return;
    const point = byBucket.get(bucket);
    const next = (point[DIMENSION_SERIES_VALUES].get(key) || 0) + totalTokens(row);
    point[DIMENSION_SERIES_VALUES].set(key, next);
  });

  const points = [...byBucket.values()].sort((a, b) =>
    String(a.bucket).localeCompare(String(b.bucket))
  );
  return { keys, points };
}

/**
 * 构建占比视图的键总计，不修改入参 rows。
 * 返回按值降序、同值按键升序的 `{ key, value }` 列表。
 */
export function buildDimensionProportionData(rows) {
  const source = Array.isArray(rows) ? rows : [];
  const totals = new Map();

  source.forEach((row) => {
    const key = row?.key;
    if (key == null) return;
    totals.set(key, (totals.get(key) || 0) + totalTokens(row));
  });

  return [...totals.entries()]
    .map(([key, value]) => ({ key, value }))
    .sort((a, b) => b.value - a.value || String(a.key).localeCompare(String(b.key)));
}

/** 构建 Top-N 排名视图数据：占比总计按降序截取前 topN 项。 */
export function buildDimensionTopData(rows, topN) {
  const limit =
    typeof topN === 'number' && Number.isFinite(topN) && topN > 0
      ? topN
      : DEFAULT_TOP_N;
  return buildDimensionProportionData(rows).slice(0, limit);
}

/**
 * 维度图 tooltip 内容：展示维度键及其 total tokens，并补充 request/quota 上下文。
 * `resolveKey` 解析当前视图下条目对应的维度键，`resolveContext` 从聚合行提供
 * 该键（趋势视图下为该 bucket+键）的请求/额度/token。独立导出以便直接单测。
 *
 * `labelKey` 是表头文案的 i18n key：趋势视图的 activeLabel 是时间 bucket，
 * 传入日期 key；Top 视图的 activeLabel 是维度键，传入维度 key；占比视图无
 * activeLabel，不传即不渲染表头。表头语义随视图而变，避免把维度键标成「日期」。
 */
export function DimensionTooltip({
  active,
  payload,
  label,
  labelKey,
  resolveKey,
  resolveContext,
}) {
  const { t } = useTranslation();
  if (!active || !payload || payload.length === 0) return null;

  return (
    <div className='dimension-tooltip' style={TOOLTIP_STYLE}>
      {label != null && labelKey && (
        <div className='dimension-tooltip-label'>
          {`${t(labelKey)}: ${label}`}
        </div>
      )}
      {payload.map((entry, index) => {
        const key = resolveKey ? resolveKey(entry, label) : entry.name;
        const context = resolveContext ? resolveContext(entry, label) : null;
        const tokens = context ? context.tokens : entry.value;
        return (
          <div className='dimension-tooltip-row' key={`${key}-${index}`}>
            <span
              className='dimension-tooltip-key'
              style={{ color: entry.color }}
            >
              {key}
            </span>
            <span>{`${t('dashboard.units.tokens')}: ${formatCount(tokens)}`}</span>
            {context && (
              <span>{`${t('dashboard.units.requests')}: ${formatCount(
                context.requests
              )}`}</span>
            )}
            {context && (
              <span>{`${t('dashboard.units.quota')}: ${renderQuota(
                context.quota
              )}`}</span>
            )}
          </div>
        );
      })}
    </div>
  );
}

function TrendView({ rows }) {
  const { keys, points } = buildDimensionTrendData(rows);
  const bucketContext = buildBucketKeyContext(rows);
  if (points.length === 0) return null;

  const resolveContext = (entry, label) => {
    const perBucket = bucketContext.get(label);
    return perBucket ? perBucket.get(entry.name) || null : null;
  };

  return (
    <ResponsiveContainer width='100%' height={300}>
      <LineChart data={points}>
        <CartesianGrid strokeDasharray='3 3' vertical={false} opacity={0.1} />
        <XAxis dataKey='bucket' axisLine={false} tickLine={false} tick={AXIS_TICK} />
        <YAxis axisLine={false} tickLine={false} tick={AXIS_TICK} />
        <Tooltip
          content={
            <DimensionTooltip
              labelKey='dashboard.statistics.tooltip.date'
              resolveKey={(entry) => entry.name}
              resolveContext={resolveContext}
            />
          }
        />
        <Legend wrapperStyle={{ paddingTop: '12px' }} />
        {keys.map((key, index) => (
          <Line
            key={key}
            type='monotone'
            dataKey={(point) => point[DIMENSION_SERIES_VALUES].get(key) || 0}
            name={key}
            stroke={colorAt(index)}
            strokeWidth={2}
            dot={false}
            activeDot={{ r: 4 }}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
  );
}

function ProportionView({ rows }) {
  const data = buildDimensionProportionData(rows);
  const keyContext = buildKeyContext(rows);
  if (data.length === 0) return null;

  return (
    <ResponsiveContainer width='100%' height={300}>
      <PieChart>
        <Pie data={data} dataKey='value' nameKey='key' outerRadius={100} label>
          {data.map((entry, index) => (
            <Cell key={entry.key} fill={colorAt(index)} />
          ))}
        </Pie>
        <Tooltip
          content={
            <DimensionTooltip
              resolveKey={(entry) => entry.name}
              resolveContext={(entry) => keyContext.get(entry.name) || null}
            />
          }
        />
        <Legend wrapperStyle={{ paddingTop: '12px' }} />
      </PieChart>
    </ResponsiveContainer>
  );
}

function TopView({ rows, topN }) {
  const data = buildDimensionTopData(rows, topN);
  const keyContext = buildKeyContext(rows);
  if (data.length === 0) return null;

  return (
    <ResponsiveContainer width='100%' height={300}>
      {/* layout='vertical'：分类轴在 Y、数值轴在 X，渲染为横向柱状图。 */}
      <BarChart data={data} layout='vertical'>
        <CartesianGrid strokeDasharray='3 3' horizontal={false} opacity={0.1} />
        <XAxis type='number' axisLine={false} tickLine={false} tick={AXIS_TICK} />
        <YAxis
          type='category'
          dataKey='key'
          axisLine={false}
          tickLine={false}
          tick={AXIS_TICK}
          width={120}
        />
        <Tooltip
          content={
            <DimensionTooltip
              labelKey='dashboard.statistics.tooltip.dimension'
              resolveKey={(entry, label) => label}
              resolveContext={(entry, label) => keyContext.get(label) || null}
            />
          }
        />
        <Bar dataKey='value' name='tokens' radius={[0, 4, 4, 0]}>
          {data.map((entry, index) => (
            <Cell key={entry.key} fill={colorAt(index)} />
          ))}
        </Bar>
      </BarChart>
    </ResponsiveContainer>
  );
}

export function DimensionChart({
  dimension,
  rows,
  view,
  topN,
  onViewChange,
  onTopNChange,
}) {
  const { t } = useTranslation();
  const source = Array.isArray(rows) ? rows : [];
  const isEmpty = source.length === 0;

  const topNOptions = TOP_N_OPTIONS.map((value) => ({
    key: value,
    value,
    text: String(value),
  }));

  return (
    <Card fluid className='chart-card dimension-chart'>
      <Card.Content>
        <Card.Header>
          {t(`dashboard.dimensions.${dimension}`)}
          <div className='dimension-chart-controls'>
            <Button.Group size='mini'>
              {['trend', 'proportion', 'top'].map((option) => (
                <Button
                  key={option}
                  active={view === option}
                  onClick={() => onViewChange && onViewChange(option)}
                >
                  {t(`dashboard.views.${option}`)}
                </Button>
              ))}
            </Button.Group>
            <Dropdown
              selection
              compact
              className='dimension-topn-dropdown'
              options={topNOptions}
              value={topN}
              onChange={(e, { value }) => onTopNChange && onTopNChange(value)}
            />
          </div>
        </Card.Header>
        <div className='chart-container' data-view={view}>
          {isEmpty ? (
            <div className='chart-empty'>{t('dashboard.empty')}</div>
          ) : view === 'proportion' ? (
            <ProportionView rows={source} />
          ) : view === 'top' ? (
            <TopView rows={source} topN={topN} />
          ) : (
            <TrendView rows={source} />
          )}
        </div>
      </Card.Content>
    </Card>
  );
}
