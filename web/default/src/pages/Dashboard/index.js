import React, { useEffect, useRef, useState } from 'react';
import { Button, Input, Message, Tab } from 'semantic-ui-react';
import { useTranslation } from 'react-i18next';
import { isAdmin } from '../../helpers';
import { fetchDashboardAggregate, fetchDashboardSummary } from './api';
import {
  loadDashboardChartPreferences,
  saveDashboardChartPreference,
} from './preferences';
import { SummaryCards } from './components/SummaryCards';
import { DimensionChart } from './components/DimensionChart';
import { TrendCharts } from './components/TrendCharts';
import './Dashboard.css';

// 共享筛选器常量：时间范围预设 1/7/14/29 天 + 自定义，粒度 hour/day/week。
// 展示文案一律经 i18n key 解析（见 dashboard.filters.*），不在此硬编码中文。
const RANGE_PRESETS = [1, 7, 14, 29];
const CUSTOM_PRESET = 'custom';
const GRANULARITIES = ['hour', 'day', 'week'];
const DEFAULT_GRANULARITY = 'day';

// 默认处于「自定义」模式，但预置一个可用的滚动窗口（最近 7 天）：
// 新的 aggregate/summary 端点要求正整数的 start_timestamp < end_timestamp，
// 空范围会被后端拒绝，因此初始状态必须携带合法区间。
const DEFAULT_PRESET = CUSTOM_PRESET;
const DEFAULT_RANGE_DAYS = 7;

const DAY_SECONDS = 24 * 60 * 60;

// 角色化分区定义：概览/模型分析/令牌分析对所有登录用户可见，渠道分析/用户分析仅管理员可见。
// 每个分区声明其聚合维度；概览复用 model 维度行来汇总请求/额度/Token 趋势，
// 因此概览 ↔ 模型分析共享同一份数据、切分区不产生额外请求。
// 分区 key 与 i18n 的 dashboard.panes.* 一一对应，展示名在渲染时解析。
const OVERVIEW_SECTION = 'overview';
const DASHBOARD_SECTIONS = [
  { key: OVERVIEW_SECTION, dimension: 'model', adminOnly: false },
  { key: 'model', dimension: 'model', adminOnly: false },
  { key: 'token', dimension: 'token', adminOnly: false },
  { key: 'channel', dimension: 'channel', adminOnly: true },
  { key: 'user', dimension: 'user', adminOnly: true },
];

// 维度图默认视图偏好由 preferences 模块提供（逐维度持久化，契约默认 {view:'trend', topN:10}）；
// 页面只持有内存态，读写存储一律经由 loadDashboardChartPreferences/saveDashboardChartPreference。
// 渲染兜底偏好：normalize 后每个维度都应有值，此处仅防御异常数据。
const FALLBACK_CHART_PREFERENCE = { view: 'trend', topN: 10 };

// 页面请求状态：成功有数据 / 成功但空 / 失败。
// 「成功但空」与「失败」必须是可区分的独立状态，页面据此分别渲染空态与错误态。
const REQUEST_READY = 'ready';
const REQUEST_EMPTY = 'empty';
const REQUEST_ERROR = 'error';

function currentSeconds() {
  return Math.floor(Date.now() / 1000);
}

// 数值预设 → 以当前时刻为终点的滚动窗口；非数值预设（自定义）返回空范围。
function resolvePresetRange(preset) {
  const days = Number(preset);
  if (!Number.isFinite(days) || days <= 0) {
    return { startTimestamp: 0, endTimestamp: 0 };
  }
  const end = currentSeconds();
  return { startTimestamp: end - days * DAY_SECONDS, endTimestamp: end };
}

// DashboardFilters 初始状态：自定义模式 + 最近 7 天 + day 粒度 + 空用户名。
function createInitialFilters() {
  const end = currentSeconds();
  return {
    rangePreset: DEFAULT_PRESET,
    startTimestamp: end - DEFAULT_RANGE_DAYS * DAY_SECONDS,
    endTimestamp: end,
    granularity: DEFAULT_GRANULARITY,
    username: '',
  };
}

// Unix 秒 → <input type="date"> 的 YYYY-MM-DD 展示值（本地时区）。
function toDateInputValue(timestampSeconds) {
  if (!timestampSeconds) return '';
  const date = new Date(timestampSeconds * 1000);
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function startOfDaySeconds(dateString) {
  return Math.floor(new Date(`${dateString}T00:00:00`).getTime() / 1000);
}

function endOfDaySeconds(dateString) {
  return Math.floor(new Date(`${dateString}T23:59:59`).getTime() / 1000);
}

export default function Dashboard() {
  const { t } = useTranslation();
  // 页面是唯一的数据所有者：聚合行与 KPI 汇总都由本页持有并下发子组件。
  // 聚合行记录其所属维度，避免切换分区时把上一维度的数据渲染到新分区。
  const [aggregate, setAggregate] = useState({
    dimension: null,
    rows: [],
    state: REQUEST_READY,
  });
  const [summary, setSummary] = useState(null);
  const [summaryState, setSummaryState] = useState(REQUEST_READY);
  // 筛选状态提升到页面顶层，位于分区容器之上，切换分区不会重置。
  const [filters, setFilters] = useState(createInitialFilters);
  const [activeSection, setActiveSection] = useState(OVERVIEW_SECTION);
  const [queryKey, setQueryKey] = useState(0);
  // 维度图视图偏好状态归属页面（按维度独立）：初值从 localStorage 恢复，
  // 子组件仅通过 props 接收与回调；每次变更同步写回存储。
  const [chartPreferences, setChartPreferences] = useState(
    loadDashboardChartPreferences
  );
  const isAdminUser = isAdmin();

  // 请求序号用于丢弃过期响应，防止快速切分区时旧维度数据覆盖新维度。
  const aggregateRequestId = useRef(0);
  const summaryRequestId = useRef(0);

  // 角色可见性：普通用户 3 个分区，管理员额外可见渠道/用户分析。
  const sections = DASHBOARD_SECTIONS.filter(
    (section) => !section.adminOnly || isAdminUser
  );
  const activeConfig =
    sections.find((section) => section.key === activeSection) || sections[0];
  const activeDimension = activeConfig.dimension;
  const activeIndex = sections.findIndex(
    (section) => section.key === activeConfig.key
  );

  const loadAggregate = async (dimension) => {
    const requestId = aggregateRequestId.current + 1;
    aggregateRequestId.current = requestId;
    try {
      const rows = await fetchDashboardAggregate({
        dimension,
        granularity: filters.granularity,
        startTimestamp: filters.startTimestamp,
        endTimestamp: filters.endTimestamp,
        username: filters.username,
      });
      if (requestId !== aggregateRequestId.current) return;
      setAggregate({
        dimension,
        rows,
        // 成功但没有任何聚合行 → 空态（与失败态区分）。
        state: rows.length === 0 ? REQUEST_EMPTY : REQUEST_READY,
      });
    } catch (error) {
      if (requestId !== aggregateRequestId.current) return;
      console.error('Failed to fetch dashboard aggregate:', error);
      setAggregate({ dimension, rows: [], state: REQUEST_ERROR });
    }
  };

  const loadSummary = async () => {
    const requestId = summaryRequestId.current + 1;
    summaryRequestId.current = requestId;
    try {
      const data = await fetchDashboardSummary({
        granularity: filters.granularity,
        startTimestamp: filters.startTimestamp,
        endTimestamp: filters.endTimestamp,
        username: filters.username,
      });
      if (requestId !== summaryRequestId.current) return;
      setSummary(data || null);
      setSummaryState(REQUEST_READY);
    } catch (error) {
      if (requestId !== summaryRequestId.current) return;
      console.error('Failed to fetch dashboard summary:', error);
      setSummary(null);
      setSummaryState(REQUEST_ERROR);
    }
  };

  // 聚合请求随「激活分区维度」变化而触发：概览与模型分析同为 model 维度，切分区不重复请求；
  // 令牌/渠道/用户分析各自触发一次对应维度的请求。查询按钮通过 queryKey 重新拉取。
  useEffect(() => {
    loadAggregate(activeDimension);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queryKey, activeDimension]);

  // KPI 汇总与维度无关，只在查询按钮触发时重新拉取，避免切分区产生无关请求。
  useEffect(() => {
    loadSummary();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queryKey]);

  const handleStatQuery = () => {
    setQueryKey((k) => k + 1);
  };

  const handleUsernameKeyDown = (e) => {
    if (e.key === 'Enter') {
      handleStatQuery();
    }
  };

  const handlePresetChange = (preset) => {
    setFilters((prev) => {
      if (preset === CUSTOM_PRESET) {
        // 切到自定义时保留当前起止，便于在已有范围上微调。
        return { ...prev, rangePreset: CUSTOM_PRESET };
      }
      return { ...prev, rangePreset: preset, ...resolvePresetRange(preset) };
    });
  };

  const handleGranularityChange = (granularity) => {
    setFilters((prev) => ({ ...prev, granularity }));
  };

  const handleStartDateChange = (value) => {
    setFilters((prev) => ({
      ...prev,
      rangePreset: CUSTOM_PRESET,
      startTimestamp: value ? startOfDaySeconds(value) : 0,
    }));
  };

  const handleEndDateChange = (value) => {
    setFilters((prev) => ({
      ...prev,
      rangePreset: CUSTOM_PRESET,
      endTimestamp: value ? endOfDaySeconds(value) : 0,
    }));
  };

  const updateChartPreference = (dimension, patch) => {
    // 基于当前内存态合成该维度的完整偏好，同步持久化后再更新页面状态。
    // 持久化只针对目标维度，其余维度保持既有偏好不被丢弃。
    const next = {
      ...(chartPreferences[dimension] || FALLBACK_CHART_PREFERENCE),
      ...patch,
    };
    saveDashboardChartPreference(dimension, next);
    setChartPreferences((prev) => ({ ...prev, [dimension]: next }));
  };

  // 三种数据状态互斥渲染：失败 → 错误态；成功但空 → 空态；其余 → 正常内容。
  const renderStateBody = (state, content) => {
    if (state === REQUEST_ERROR) {
      return (
        <Message negative className='dashboard-error'>
          {t('dashboard.error')}
        </Message>
      );
    }
    if (state === REQUEST_EMPTY) {
      return (
        <div className='chart-empty dashboard-empty'>
          {t('dashboard.empty')}
        </div>
      );
    }
    return content;
  };

  // 仅当已加载的聚合行属于当前分区维度时才渲染，避免跨维度显示陈旧数据。
  const aggregateMatchesActive = aggregate.dimension === activeDimension;
  const rows = aggregateMatchesActive ? aggregate.rows : [];
  const activeAggregateState = aggregateMatchesActive
    ? aggregate.state
    : REQUEST_READY;
  // 概览额外依赖 KPI 汇总；汇总失败时概览呈现错误态。
  const overviewState =
    activeAggregateState === REQUEST_ERROR || summaryState === REQUEST_ERROR
      ? REQUEST_ERROR
      : activeAggregateState;

  const renderSectionContent = (section) => {
    if (section.key === OVERVIEW_SECTION) {
      return renderStateBody(
        overviewState,
        <>
          <SummaryCards summary={summary} />
          <TrendCharts rows={rows} granularity={filters.granularity} />
        </>
      );
    }

    const preference =
      chartPreferences[section.dimension] || FALLBACK_CHART_PREFERENCE;
    return renderStateBody(
      activeAggregateState,
      <DimensionChart
        dimension={section.dimension}
        rows={rows}
        view={preference.view}
        topN={preference.topN}
        onViewChange={(view) =>
          updateChartPreference(section.dimension, { view })
        }
        onTopNChange={(topN) =>
          updateChartPreference(section.dimension, { topN })
        }
      />
    );
  };

  // semantic-ui Tab 只渲染激活分区（renderActiveOnly 默认 true），
  // data-section 用于测试与定位，挂在菜单项上。
  const panes = sections.map((section) => ({
    menuItem: {
      key: section.key,
      content: t(`dashboard.panes.${section.key}`),
      'data-section': section.key,
    },
    render: () => (
      <Tab.Pane attached={false}>{renderSectionContent(section)}</Tab.Pane>
    ),
  }));

  const isCustomRange = filters.rangePreset === CUSTOM_PRESET;

  return (
    <div className='dashboard-container'>
      {/* 筛选器位于分区容器之上：切换分区不会重置预设/自定义范围/粒度/用户名。 */}
      <div className='dashboard-filters'>
        {isAdminUser && (
          <Input
            size='small'
            className='dashboard-username-input'
            value={filters.username}
            onChange={(e) =>
              setFilters((prev) => ({ ...prev, username: e.target.value }))
            }
            onKeyDown={handleUsernameKeyDown}
            placeholder={t('dashboard.filters.username')}
            style={{ width: '200px' }}
          />
        )}

        <span className='dashboard-filter-label'>
          {t('dashboard.filters.range')}
        </span>
        <Button.Group size='mini' className='dashboard-preset-group'>
          {RANGE_PRESETS.map((preset) => (
            <Button
              key={preset}
              active={filters.rangePreset === preset}
              data-preset={preset}
              onClick={() => handlePresetChange(preset)}
            >
              {t(`dashboard.filters.presets.${preset}`)}
            </Button>
          ))}
          <Button
            active={isCustomRange}
            data-preset={CUSTOM_PRESET}
            onClick={() => handlePresetChange(CUSTOM_PRESET)}
          >
            {t('dashboard.filters.presets.custom')}
          </Button>
        </Button.Group>

        {isCustomRange && (
          <>
            <label className='dashboard-filter-label'>
              {t('dashboard.filters.start')}
            </label>
            <input
              type='date'
              className='dashboard-date-input'
              value={toDateInputValue(filters.startTimestamp)}
              onChange={(e) => handleStartDateChange(e.target.value)}
            />
            <label className='dashboard-filter-label'>
              {t('dashboard.filters.end')}
            </label>
            <input
              type='date'
              className='dashboard-date-input'
              value={toDateInputValue(filters.endTimestamp)}
              onChange={(e) => handleEndDateChange(e.target.value)}
            />
          </>
        )}

        <span className='dashboard-filter-label'>
          {t('dashboard.filters.granularity')}
        </span>
        <Button.Group size='mini' className='dashboard-granularity-group'>
          {GRANULARITIES.map((granularity) => (
            <Button
              key={granularity}
              active={filters.granularity === granularity}
              data-granularity={granularity}
              onClick={() => handleGranularityChange(granularity)}
            >
              {t(`dashboard.filters.granularities.${granularity}`)}
            </Button>
          ))}
        </Button.Group>

        <Button size='small' primary onClick={handleStatQuery}>
          {t('dashboard.filters.query')}
        </Button>
      </div>

      <div
        className='dashboard-sections'
        data-active-section={activeConfig.key}
      >
        <Tab
          menu={{
            secondary: true,
            pointing: true,
            className: 'dashboard-tabs',
          }}
          panes={panes}
          activeIndex={activeIndex}
          onTabChange={(e, { activeIndex: nextIndex }) => {
            const next = sections[nextIndex];
            if (next) setActiveSection(next.key);
          }}
        />
      </div>
    </div>
  );
}
