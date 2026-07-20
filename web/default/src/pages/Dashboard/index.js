import React, {useEffect, useState} from 'react';
import {useTranslation} from 'react-i18next';
import {Button, Card, Grid, Input} from 'semantic-ui-react';
import {
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import axios from 'axios';
import {isAdmin} from '../../helpers';
import './Dashboard.css';

// 在 Dashboard 组件内添加自定义配置
const chartConfig = {
  lineChart: {
    style: {
      background: '#fff',
      borderRadius: '8px',
    },
    line: {
      strokeWidth: 2,
      dot: false,
      activeDot: { r: 4 },
    },
    grid: {
      vertical: false,
      horizontal: true,
      opacity: 0.1,
    },
  },
  colors: {
    requests: '#4318FF',
    quota: '#00B5D8',
    tokens: '#6C63FF',
  },
  barColors: [
    '#4318FF', // 深紫色
    '#00B5D8', // 青色
    '#6C63FF', // 紫色
    '#05CD99', // 绿色
    '#FFB547', // 橙色
    '#FF5E7D', // 粉色
    '#41B883', // 翠绿
    '#7983FF', // 淡紫
    '#FF8F6B', // 珊瑚色
    '#49BEFF', // 天蓝
  ],
};

const Dashboard = () => {
  const { t } = useTranslation();
  const [data, setData] = useState([]);
  const [summaryData, setSummaryData] = useState({
    todayRequests: 0,
    todayQuota: 0,
    todayTokens: 0,
  });
  const [username, setUsername] = useState('');
  const [startDate, setStartDate] = useState('');
  const [endDate, setEndDate] = useState('');
  const [queryKey, setQueryKey] = useState(0);
  const isAdminUser = isAdmin();

  useEffect(() => {
    fetchDashboardData();
  }, [queryKey]);

  const fetchDashboardData = async () => {
    try {
      let url = '/api/user/dashboard';
      const params = [];
      if (isAdminUser && username) {
        params.push(`username=${encodeURIComponent(username)}`);
      }
      if (startDate) {
        params.push(`start_timestamp=${Math.floor(new Date(startDate).getTime() / 1000)}`);
      }
      if (endDate) {
        // 结束日期设为当天最后一秒
        const end = new Date(endDate);
        end.setHours(23, 59, 59, 999);
        params.push(`end_timestamp=${Math.floor(end.getTime() / 1000)}`);
      }
      if (params.length > 0) {
        url += '?' + params.join('&');
      }
      const response = await axios.get(url);
      if (response.data.success) {
        const dashboardData = response.data.data || [];
        setData(dashboardData);
        calculateSummary(dashboardData);
      }
    } catch (error) {
      console.error('Failed to fetch dashboard data:', error);
      setData([]);
      calculateSummary([]);
    }
  };

  const calculateSummary = (dashboardData) => {
    if (!Array.isArray(dashboardData) || dashboardData.length === 0) {
      setSummaryData({
        todayRequests: 0,
        todayQuota: 0,
        todayTokens: 0,
      });
      return;
    }

    const today = new Date().toISOString().split('T')[0];
    const todayData = dashboardData.filter((item) => item.Day === today);

    const summary = {
      todayRequests: todayData.reduce(
        (sum, item) => sum + item.RequestCount,
        0
      ),
      todayQuota:
        todayData.reduce((sum, item) => sum + item.Quota, 0) / 1000000,
      todayTokens: todayData.reduce(
        (sum, item) => sum + item.PromptTokens + item.CompletionTokens,
        0
      ),
    };

    setSummaryData(summary);
  };

  const handleStatQuery = () => {
    setQueryKey((k) => k + 1);
  };

  const handleUsernameKeyDown = (e) => {
    if (e.key === 'Enter') {
      handleStatQuery();
    }
  };

  // 处理数据以供折线图使用，补充缺失的日期
  const processTimeSeriesData = () => {
    const dailyData = {};

    // 获取日期范围
    const dates = data.map((item) => item.Day);
    const maxDate =
      dates.length > 0
        ? new Date(Math.max(...dates.map((d) => new Date(d))))
        : new Date();
    let minDate =
      dates.length > 0
        ? new Date(Math.min(...dates.map((d) => new Date(d))))
        : new Date();

    // 确保至少显示7天的数据
    const sevenDaysAgo = new Date();
    sevenDaysAgo.setDate(sevenDaysAgo.getDate() - 6); // -6是因为包含今天
    if (minDate > sevenDaysAgo) {
      minDate = sevenDaysAgo;
    }

    // 生成所有日期
    for (let d = new Date(minDate); d <= maxDate; d.setDate(d.getDate() + 1)) {
      const dateStr = d.toISOString().split('T')[0];
      dailyData[dateStr] = {
        date: dateStr,
        requests: 0,
        quota: 0,
        tokens: 0,
      };
    }

    // 填充实际数据
    data.forEach((item) => {
      dailyData[item.Day].requests += item.RequestCount;
      dailyData[item.Day].quota += item.Quota / 1000000;
      dailyData[item.Day].tokens += item.PromptTokens + item.CompletionTokens;
    });

    return Object.values(dailyData).sort((a, b) =>
      a.date.localeCompare(b.date)
    );
  };

  // 处理数据以供堆叠柱状图使用
  const processModelData = () => {
    const timeData = {};

    // 获取日期范围
    const dates = data.map((item) => item.Day);
    const maxDate =
      dates.length > 0
        ? new Date(Math.max(...dates.map((d) => new Date(d))))
        : new Date();
    let minDate =
      dates.length > 0
        ? new Date(Math.min(...dates.map((d) => new Date(d))))
        : new Date();

    // 确保至少显示7天的数据
    const sevenDaysAgo = new Date();
    sevenDaysAgo.setDate(sevenDaysAgo.getDate() - 6); // -6是因为包含今天
    if (minDate > sevenDaysAgo) {
      minDate = sevenDaysAgo;
    }

    // 生成所有日期
    for (let d = new Date(minDate); d <= maxDate; d.setDate(d.getDate() + 1)) {
      const dateStr = d.toISOString().split('T')[0];
      timeData[dateStr] = {
        date: dateStr,
      };

      // 初始化所有模型的数据为0
      const models = [...new Set(data.map((item) => item.ModelName))];
      models.forEach((model) => {
        timeData[dateStr][model] = 0;
      });
    }

    // 填充实际数据
    data.forEach((item) => {
      timeData[item.Day][item.ModelName] =
        item.PromptTokens + item.CompletionTokens;
    });

    return Object.values(timeData).sort((a, b) => a.date.localeCompare(b.date));
  };

  // 获取所有唯一的模型名称
  const getUniqueModels = () => {
    return [...new Set(data.map((item) => item.ModelName))];
  };

  const timeSeriesData = processTimeSeriesData();
  const modelData = processModelData();
  const models = getUniqueModels();

  // 生成随机颜色
  const getRandomColor = (index) => {
    return chartConfig.barColors[index % chartConfig.barColors.length];
  };

  // 添加一个日期格式化函数
  const formatDate = (dateStr) => {
    const date = new Date(dateStr);
    return date.toLocaleDateString('zh-CN', {
      month: 'numeric',
      day: 'numeric',
    });
  };

  // 修改所有 XAxis 配置
  const xAxisConfig = {
    dataKey: 'date',
    axisLine: false,
    tickLine: false,
    tick: {
      fontSize: 12,
      fill: '#A3AED0',
      textAnchor: 'middle', // 文本居中对齐
    },
    tickFormatter: formatDate,
    interval: 0,
    minTickGap: 5,
    padding: { left: 30, right: 30 }, // 增加两侧的内边距，确保首尾标签完整显示
  };

  return (
    <div className='dashboard-container'>
      <div style={{ marginBottom: '16px', display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' }}>
        {isAdminUser && (
          <Input
            size='small'
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            onKeyDown={handleUsernameKeyDown}
            placeholder='用户名（留空=全部用户）'
            style={{ width: '200px' }}
          />
        )}
        <label style={{ fontSize: '13px', color: '#A3AED0', whiteSpace: 'nowrap' }}>起</label>
        <input
          type='date'
          value={startDate}
          onChange={(e) => setStartDate(e.target.value)}
          style={{
            padding: '6px 10px',
            border: '1px solid #e0e0e0',
            borderRadius: '4px',
            fontSize: '13px',
            outline: 'none',
          }}
        />
        <label style={{ fontSize: '13px', color: '#A3AED0', whiteSpace: 'nowrap' }}>止</label>
        <input
          type='date'
          value={endDate}
          onChange={(e) => setEndDate(e.target.value)}
          style={{
            padding: '6px 10px',
            border: '1px solid #e0e0e0',
            borderRadius: '4px',
            fontSize: '13px',
            outline: 'none',
          }}
        />
        <Button size='small' primary onClick={handleStatQuery}>
          查询
        </Button>
      </div>
      {/* 三个并排的折线图 */}
      <Grid columns={3} stackable className='charts-grid'>
        <Grid.Column>
          <Card fluid className='chart-card'>
            <Card.Content>
              <Card.Header>
                {t('dashboard.charts.requests.title')}
                {/* <span className='stat-value'>{summaryData.todayRequests}</span> */}
              </Card.Header>
              <div className='chart-container'>
                <ResponsiveContainer
                  width='100%'
                  height={120}
                  margin={{ left: 10, right: 10 }} // 调整容器边距
                >
                  <LineChart data={timeSeriesData}>
                    <CartesianGrid
                      strokeDasharray='3 3'
                      vertical={chartConfig.lineChart.grid.vertical}
                      horizontal={chartConfig.lineChart.grid.horizontal}
                      opacity={chartConfig.lineChart.grid.opacity}
                    />
                    <XAxis {...xAxisConfig} />
                    <YAxis hide={true} />
                    <Tooltip
                      contentStyle={{
                        background: '#fff',
                        border: 'none',
                        borderRadius: '4px',
                        boxShadow: '0 2px 8px rgba(0,0,0,0.1)',
                      }}
                      formatter={(value) => [
                        value,
                        t('dashboard.charts.requests.tooltip'),
                      ]}
                      labelFormatter={(label) =>
                        `${t(
                          'dashboard.statistics.tooltip.date'
                        )}: ${formatDate(label)}`
                      }
                    />
                    <Line
                      type='monotone'
                      dataKey='requests'
                      stroke={chartConfig.colors.requests}
                      strokeWidth={chartConfig.lineChart.line.strokeWidth}
                      dot={chartConfig.lineChart.line.dot}
                      activeDot={chartConfig.lineChart.line.activeDot}
                    />
                  </LineChart>
                </ResponsiveContainer>
              </div>
            </Card.Content>
          </Card>
        </Grid.Column>

        <Grid.Column>
          <Card fluid className='chart-card'>
            <Card.Content>
              <Card.Header>
                {t('dashboard.charts.quota.title')}
                {/* <span className='stat-value'>
                  ${summaryData.todayQuota.toFixed(3)}
                </span> */}
              </Card.Header>
              <div className='chart-container'>
                <ResponsiveContainer
                  width='100%'
                  height={120}
                  margin={{ left: 10, right: 10 }} // 调整容器边距
                >
                  <LineChart data={timeSeriesData}>
                    <CartesianGrid
                      strokeDasharray='3 3'
                      vertical={chartConfig.lineChart.grid.vertical}
                      horizontal={chartConfig.lineChart.grid.horizontal}
                      opacity={chartConfig.lineChart.grid.opacity}
                    />
                    <XAxis {...xAxisConfig} />
                    <YAxis hide={true} />
                    <Tooltip
                      contentStyle={{
                        background: '#fff',
                        border: 'none',
                        borderRadius: '4px',
                        boxShadow: '0 2px 8px rgba(0,0,0,0.1)',
                      }}
                      formatter={(value) => [
                        value.toFixed(6),
                        t('dashboard.charts.quota.tooltip'),
                      ]}
                      labelFormatter={(label) =>
                        `${t(
                          'dashboard.statistics.tooltip.date'
                        )}: ${formatDate(label)}`
                      }
                    />
                    <Line
                      type='monotone'
                      dataKey='quota'
                      stroke={chartConfig.colors.quota}
                      strokeWidth={chartConfig.lineChart.line.strokeWidth}
                      dot={chartConfig.lineChart.line.dot}
                      activeDot={chartConfig.lineChart.line.activeDot}
                    />
                  </LineChart>
                </ResponsiveContainer>
              </div>
            </Card.Content>
          </Card>
        </Grid.Column>

        <Grid.Column>
          <Card fluid className='chart-card'>
            <Card.Content>
              <Card.Header>
                {t('dashboard.charts.tokens.title')}
                {/* <span className='stat-value'>{summaryData.todayTokens}</span> */}
              </Card.Header>
              <div className='chart-container'>
                <ResponsiveContainer
                  width='100%'
                  height={120}
                  margin={{ left: 10, right: 10 }} // 调整容器边距
                >
                  <LineChart data={timeSeriesData}>
                    <CartesianGrid
                      strokeDasharray='3 3'
                      vertical={chartConfig.lineChart.grid.vertical}
                      horizontal={chartConfig.lineChart.grid.horizontal}
                      opacity={chartConfig.lineChart.grid.opacity}
                    />
                    <XAxis {...xAxisConfig} />
                    <YAxis hide={true} />
                    <Tooltip
                      contentStyle={{
                        background: '#fff',
                        border: 'none',
                        borderRadius: '4px',
                        boxShadow: '0 2px 8px rgba(0,0,0,0.1)',
                      }}
                      formatter={(value) => [
                        value,
                        t('dashboard.charts.tokens.tooltip'),
                      ]}
                      labelFormatter={(label) =>
                        `${t(
                          'dashboard.statistics.tooltip.date'
                        )}: ${formatDate(label)}`
                      }
                    />
                    <Line
                      type='monotone'
                      dataKey='tokens'
                      stroke={chartConfig.colors.tokens}
                      strokeWidth={chartConfig.lineChart.line.strokeWidth}
                      dot={chartConfig.lineChart.line.dot}
                      activeDot={chartConfig.lineChart.line.activeDot}
                    />
                  </LineChart>
                </ResponsiveContainer>
              </div>
            </Card.Content>
          </Card>
        </Grid.Column>
      </Grid>

      {/* 模型使用统计 */}
      <Card fluid className='chart-card'>
        <Card.Content>
          <Card.Header>{t('dashboard.statistics.title')}</Card.Header>
          <div className='chart-container'>
            <ResponsiveContainer width='100%' height={300}>
              <BarChart data={modelData}>
                <CartesianGrid
                  strokeDasharray='3 3'
                  vertical={false}
                  opacity={0.1}
                />
                <XAxis {...xAxisConfig} />
                <YAxis
                  axisLine={false}
                  tickLine={false}
                  tick={{ fontSize: 12, fill: '#A3AED0' }}
                />
                <Tooltip
                  content={({active, payload, label}) => {
                    if (!active || !payload) return null;
                    const filtered = payload.filter(
                      (item) => item.value !== 0
                    );
                    if (filtered.length === 0) return null;
                    return (
                      <div
                        style={{
                          background: '#fff',
                          border: 'none',
                          borderRadius: '4px',
                          boxShadow: '0 2px 8px rgba(0,0,0,0.1)',
                          padding: '8px 12px',
                        }}
                      >
                        <p style={{margin: '0 0 6px', fontWeight: 600}}>
                          {t('dashboard.statistics.tooltip.date')}:{' '}
                          {formatDate(label)}
                        </p>
                        {filtered.map((entry, idx) => (
                          <p
                            key={idx}
                            style={{
                              color: entry.color,
                              margin: '2px 0',
                              fontSize: 13,
                            }}
                          >
                            {entry.name}: {entry.value.toLocaleString()}
                          </p>
                        ))}
                      </div>
                    );
                  }}
                />
                <Legend
                  wrapperStyle={{
                    paddingTop: '20px',
                  }}
                />
                {models.map((model, index) => (
                  <Bar
                    key={model}
                    dataKey={model}
                    stackId='a'
                    fill={getRandomColor(index)}
                    name={model}
                    radius={[4, 4, 0, 0]}
                  />
                ))}
              </BarChart>
            </ResponsiveContainer>
          </div>
        </Card.Content>
      </Card>
    </div>
  );
};

export default Dashboard;
