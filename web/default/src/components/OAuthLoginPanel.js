import React, { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Icon, Message, Progress, Segment } from 'semantic-ui-react';
import { API, showError, showSuccess, showWarning } from '../helpers';
import { descriptorOAuth, pickI18n } from '../helpers/channelDescriptor';

// 通用 OAuth 登录面板（PRD §5.12 层2 / 决策 D7）。
//
// 取代原渠道专用面板。两条流程在前端
// **完全同形**——在新窗口打开授权 URL + 轮询 session 取凭证，差异仅在 descriptor 下发的
// OAuth 元数据（start/poll 路径与 start 请求参数）。因此本组件不含任何渠道专属分支，
// 全部行为由 descriptor 驱动。
//
// 状态机（与旧面板逐条对齐）：
//   idle ──点击──▶ requesting ──▶ waiting（轮询中，显示 authUrl + 已等待时长）
//                       │                │
//                       ▼                ├──成功──▶ done（凭证回填密钥框）
//                      error             ├──超时（5 分钟）──▶ timeout
//                                        └──用户取消──▶ idle
//
// 设计要点：登录成功后**只把凭证交给父组件回填密钥框，不直接写库**（§8.4）——新建渠道时尚无
// ID，统一走「后端返回凭证 → 前端填 inputs.key → 用户点保存」可以让新建与编辑走完全相同的
// 路径。因此密钥框必须保持可手写（方案 A 是降级路径）。

// 轮询间隔（§8.1「每 2s 轮询」）。
const POLL_INTERVAL_MS = 2000;
// 轮询总时长上限（§8.5「超时缺失」）：用户关掉授权页后前端必须主动放弃，否则会永久轮询。
const POLL_TIMEOUT_MS = 5 * 60 * 1000;
// 单次 poll 请求的**局部**超时：35s，略大于后端上游调用的 30s。
//
// 为什么必须显式配：helpers/api.js 的 API 实例只设了 baseURL、没有 timeout，即请求可以永不
// settle。那样 inFlightRef 会被一个在飞的旧 poll 永久占住，即使 reset/start 已重置标志，
// 下一次 pollOnce 也会因「上一个请求还没落地」而永远不发出（表现为「重新登录后面板毫无反应」）。
// 取 35s 而不是更短：短于后端的 30s 会在上游正常但偏慢时把一次本可成功的换取打断。
const POLL_REQUEST_TIMEOUT_MS = 35000;

// 上游抖动容忍阈值：连续 N 次「5xx / 网络错误 / 请求超时」才把轮询判为终态失败。
//
// 后端 poll 在「上游不可达 / 上游 5xx / 解析失败」时返回 **502**，属于可恢复的暂时故障；
// 单次抖动不应打断整个登录流程（否则用户得把授权流程重走一遍）。但也不能无限容忍：连续 3 次
// 足以过滤偶发抖动，又能在上游真正宕机时及时收口（快失败约 3×2s；慢超时约 3×35s）。
// 退避策略为**固定间隔**：直接复用既有的 2s 轮询定时器，失败后不停表，下一次 tick 自然重试。
const POLL_MAX_TRANSIENT_RETRIES = 3;

// 判定「可容忍的上游抖动」：无 HTTP 响应（网络错误 / 请求超时 / 被取消）或 5xx。
// 其余（410 及各种 4xx）由调用方按各自语义单独处理。
function isTransientPollError(error) {
  const status = error?.response?.status;
  if (status === undefined || status === null) return true;
  return status >= 500 && status <= 599;
}

// toApiPath 把 descriptor 下发的相对路径（如 /channel/ext/login/start）补成 /api 前缀。
// 已是 /api/... 的路径原样返回，避免二次前缀。
function toApiPath(path) {
  if (!path) return '';
  const p = path.startsWith('/') ? path : `/${path}`;
  return p.startsWith('/api/') ? p : `/api${p}`;
}

const OAuthLoginPanel = ({ descriptor, onCredential, disabled }) => {
  const { t, i18n } = useTranslation();
  const lang = i18n.language;

  const oauth = descriptorOAuth(descriptor) || {};
  const startPath = oauth.start_path || '';
  const pollPath = oauth.poll_path || '';
  // params 每次渲染都是新对象，用其 JSON 签名做依赖，避免 effect / 定时器被无谓重建。
  const paramsSig = JSON.stringify(oauth.params || {});
  // 面板身份 = 登录路径 + start 参数。身份变化（如切换渠道类型导致 realm 变化）时作废进行中的会话。
  const identity = `${startPath}|${pollPath}|${paramsSig}`;

  const [phase, setPhase] = useState('idle'); // idle|requesting|waiting|done|timeout|error
  const [authUrl, setAuthUrl] = useState('');
  const [elapsed, setElapsed] = useState(0);
  const [errorText, setErrorText] = useState('');
  // 当前连续容忍的上游抖动次数（>0 时面板显示「正在重试」）。仅用于展示，逻辑判定走 pollFailuresRef。
  const [retryCount, setRetryCount] = useState(0);

  // 定时器 id 存 ref：轮询的生命周期跨越多次渲染，放 state 会读到过期值（§8.5「轮询泄漏」）。
  const timerRef = useRef(null);
  const popupRef = useRef(null);
  const startedAtRef = useRef(0);
  const sessionIdRef = useRef('');
  // 在飞标志：pollOnce 是 async，后端单次 poll 最长挂 30s，而间隔只有 2s，
  // 没有它最多会叠十几个并发 poll（§8.5「轮询泄漏」的另一种形态）。
  const inFlightRef = useRef(false);
  // 连续容忍失败计数：放 ref 便于在 async 轮询回调里读最新值，不受闭包过期影响。
  const pollFailuresRef = useRef(0);
  // 当前 descriptor 的登录配置（路径 + start 参数）。放 ref 让回调读到最新值，又不必把每次
  // 渲染都新建的 params 对象塞进 useCallback 依赖（否则回调与定时器会被无谓重建）。
  const configRef = useRef({ startPath, pollPath, params: {} });
  useEffect(() => {
    configRef.current = { startPath, pollPath, params: oauth.params || {} };
    // paramsSig 编码了 oauth.params，用它做依赖即可覆盖参数变化。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [startPath, pollPath, paramsSig]);

  // 停止轮询。必须在**五条路径**上都调用：组件卸载 / 成功 / 超时 / 取消 / 出错（§8.5）。
  const stopPolling = useCallback(() => {
    if (timerRef.current !== null) {
      clearInterval(timerRef.current);
      timerRef.current = null;
    }
  }, []);

  const reset = useCallback(
    (nextPhase) => {
      stopPolling();
      // 在飞标志必须与定时器一起清零：取消 / 超时 / 切类型 / 卸载这四条路径都只是
      // 「不再需要这次会话」，但**不会**去 settle 那个已经在飞的请求。若在这里留着
      // inFlightRef=true，旧请求未落地期间新会话的每一次 pollOnce 都会直接 return，
      // 表现为「重新登录后面板毫无反应」，最长卡死到 POLL_TIMEOUT_MS。
      inFlightRef.current = false;
      sessionIdRef.current = '';
      startedAtRef.current = 0;
      popupRef.current = null;
      // 会话作废时一并清掉重试计数，避免旧会话的失败次数拖累新会话。
      pollFailuresRef.current = 0;
      setRetryCount(0);
      setElapsed(0);
      setPhase(nextPhase);
    },
    [stopPolling]
  );

  // 路径 ①：组件卸载（切页 / 渠道类型切换导致本组件被卸载）。
  // 除停表外还要作废会话身份与在飞标志：卸载瞬间若恰有一个 poll 在飞，它落地后仅靠
  // sid !== sessionIdRef.current 才能被拦下（置空 sessionIdRef 使该防线生效），
  // 否则会穿透到父组件 setInputs 与 toast。此处只清 ref，不碰 state。
  useEffect(
    () => () => {
      stopPolling();
      sessionIdRef.current = '';
      inFlightRef.current = false;
    },
    [stopPolling]
  );

  // 面板身份变化（渠道类型被改掉 → 登录路径 / 参数已变）时，进行中的登录会话立即作废
  // （旧 session 换来的凭证属于另一个域 / 渠道）。
  const identityRef = useRef(identity);
  useEffect(() => {
    if (identityRef.current !== identity) {
      identityRef.current = identity;
      reset('idle');
      setAuthUrl('');
    }
  }, [identity, reset]);

  const pollOnce = useCallback(async () => {
    // 在飞保护：同一时刻只允许一个 poll 在飞。
    // 否则慢上游下会叠多个并发 poll：先返回的那个成功填入凭证（phase=done），随后返回的
    // 那个必然拿到 410（session 成功后已被后端删除），其 catch 会把 done 覆盖成 timeout
    // 并弹「登录会话已过期」的误导提示。
    if (inFlightRef.current) return;
    inFlightRef.current = true;
    try {
      // 坑 3：超时上限 —— 到点主动放弃，不无限轮询。
      const waited = Date.now() - startedAtRef.current;
      if (waited >= POLL_TIMEOUT_MS) {
        reset('timeout');
        showWarning(t('channel.edit.oauth.messages.timeout'));
        return;
      }
      setElapsed(waited);
      // 坑 5：弹窗被用户关闭 —— 提示而不是静默等待。
      if (popupRef.current && popupRef.current.closed) {
        reset('error');
        setErrorText(t('channel.edit.oauth.messages.popup_closed'));
        showWarning(t('channel.edit.oauth.messages.popup_closed'));
        return;
      }
      // 请求发起时**捕获会话身份**：响应落地时若身份已变，说明这份响应属于旧会话
      // （已取消 / 已超时 / 已切类型 / 已成功，或用户已开启新会话），必须整份丢弃。
      //
      // 只判「sessionIdRef 非空」是不够的：session A 的 poll 在飞 → 用户取消 → 立即
      // start B → A 的响应回来时 sessionIdRef 是 B（非空），旧的非空判定会放行，于是
      // 面板弹成功提示并把 **A 的凭证**填进密钥框，而 B 被孤立。比对身份才能堵住串号。
      const sid = sessionIdRef.current;
      try {
        const res = await API.get(
          `${toApiPath(configRef.current.pollPath)}?sessionId=${encodeURIComponent(
            sid
          )}`,
          { timeout: POLL_REQUEST_TIMEOUT_MS }
        );
        const data = res.data || {};
        // 终态 / 跨会话保护：身份已变 → 这份响应已过期，一律丢弃，绝不用它覆盖终态。
        if (sid !== sessionIdRef.current) return;
        // 请求成功落地（无论 pending 还是 ok）→ 之前的连续抖动计数清零。
        if (pollFailuresRef.current !== 0) {
          pollFailuresRef.current = 0;
          setRetryCount(0);
        }
        if (data.status === 'pending') {
          // 202：用户还没在浏览器里完成登录，继续轮询。
          return;
        }
        if (data.status === 'ok') {
          // 路径 ②：成功 —— 先停表再回调，回调里若触发父组件重渲染也不会漏停。
          stopPolling();
          sessionIdRef.current = '';
          popupRef.current = null;
          setPhase('done');
          if (data.warning) {
            showWarning(data.warning);
          }
          showSuccess(t('channel.edit.oauth.messages.success'));
          if (typeof onCredential === 'function') {
            // 必须输出紧凑单行（不能 pretty）：渠道 Key 会落进 channels.key，而 AddChannel 按
            // '\n' 拆分批量录入 —— 带缩进的多行凭证会被逐行拆成十几条渠道记录。
            onCredential(JSON.stringify(data.credential));
          }
          return;
        }
        // 未知状态：按错误处理，避免静默卡在轮询里。
        stopPolling();
        setPhase('error');
        setErrorText(t('channel.edit.oauth.messages.poll_failed'));
      } catch (error) {
        // 终态 / 跨会话保护（同上）：已经被成功 / 取消 / 超时终结的会话，其迟到的失败
        // （典型是 410）不得再改写 phase，也不得再弹「会话已过期」的误导提示。
        //
        // ⚠️ 判定必须**先于** stopPolling：若先停表，旧会话的迟到失败会清掉**新会话**
        // 刚起的定时器，把新会话一起拖死（与 inFlightRef 未重置同型的静默卡死）。
        if (sid !== sessionIdRef.current) return;

        // 坑 4：410 = session 过期/已用过 —— 立即终态，提示重试（防 state 复用）。行为保持不变。
        const status = error?.response?.status;
        if (status === 410) {
          stopPolling();
          setPhase('timeout');
          setErrorText(t('channel.edit.oauth.messages.expired'));
          showWarning(t('channel.edit.oauth.messages.expired'));
          return;
        }

        // 上游抖动（5xx / 网络错误 / 请求超时）—— 有限次容忍，不立刻打断登录流程。
        //
        // 中间态**不调用 showError**：helpers/api.js 的全局响应拦截器已对每个错误响应弹过
        // 一条 toast，这里再弹会叠加；且每失败一次弹一条会把「正在重试」刷成一片红。重试期间
        // 的状态改由面板内的 status_retrying 文案反馈。只有超过阈值进入终态时才补一条显式提示。
        if (isTransientPollError(error)) {
          pollFailuresRef.current += 1;
          const failures = pollFailuresRef.current;
          if (failures <= POLL_MAX_TRANSIENT_RETRIES) {
            // 不停表：下一次 2s tick 自动重试（固定间隔退避）。inFlightRef 会在 finally 清零，
            // 不会因为重试而叠并发请求。
            setRetryCount(failures);
            return;
          }
          // 连续失败超过阈值 → 收口为终态。
          stopPolling();
          setPhase('error');
          setErrorText(t('channel.edit.oauth.messages.poll_unstable'));
          showError(error);
          return;
        }

        // 路径 ⑤：其余错误（各类 4xx 客户端错误等）按终态处理。
        stopPolling();
        setPhase('error');
        setErrorText(
          error?.response?.data?.error ||
            error?.message ||
            t('channel.edit.oauth.messages.poll_failed')
        );
        showError(error);
      }
    } finally {
      inFlightRef.current = false;
    }
  }, [onCredential, reset, stopPolling, t]);

  const start = useCallback(async () => {
    if (disabled) return;
    setErrorText('');
    setPhase('requesting');

    // 坑 1：弹窗拦截 —— window.open 必须在**用户点击的同步回调**里调用。
    // 先开一个 about:blank 占位窗口（同步），等接口返回 authUrl 后再导航过去；
    // 若先 await 再 window.open，浏览器会判定为非用户手势而拦截。
    const win = typeof window !== 'undefined' ? window.open('about:blank', '_blank') : null;
    popupRef.current = win;
    if (!win) {
      showWarning(t('channel.edit.oauth.messages.popup_blocked'));
    }

    try {
      // start 请求体由 descriptor 的 OAuth 参数下发（各扩展渠道按需下发参数，callback_url 由后端按 Host 推导）。
      const res = await API.post(toApiPath(configRef.current.startPath), {
        ...configRef.current.params,
      });
      const data = res.data || {};
      if (!data.sessionId || !data.authUrl) {
        setPhase('error');
        setErrorText(t('channel.edit.oauth.messages.start_failed'));
        return;
      }
      // 双保险：新会话开始前再清一次在飞标志。reset() 已经覆盖取消 / 超时 / 切类型，
      // 这里补上「start 与 reset 之间存在在飞请求」的时序（例如取消后立刻重新 start，
      // 旧请求尚未落地），确保新会话的第一次轮询不会被上一个会话的请求挡住。
      inFlightRef.current = false;
      sessionIdRef.current = data.sessionId;
      startedAtRef.current = Date.now();
      // 新会话开始：清空上一个会话遗留的重试计数。
      pollFailuresRef.current = 0;
      setRetryCount(0);
      setAuthUrl(data.authUrl);
      setElapsed(0);
      // 授权页导航：占位窗口已被同步创建，这里再赋值不会被拦截。
      if (win && !win.closed) {
        win.location.href = data.authUrl;
      }
      setPhase('waiting');
      stopPolling();
      timerRef.current = setInterval(() => {
        pollOnce();
      }, POLL_INTERVAL_MS);
    } catch (error) {
      // 路径 ③（start 失败）：关掉占位窗口，回到 error。
      // stopPolling 是防御性的：本分支走到时必然没有在跑的定时器（还没 setInterval），
      // 但保留它才能让「注释里的五条路径都停表」与实现对得上，后续插入重试也不会漏停。
      stopPolling();
      if (win && !win.closed) {
        win.close();
      }
      popupRef.current = null;
      setPhase('error');
      setErrorText(
        error?.response?.data?.error ||
          error?.message ||
          t('channel.edit.oauth.messages.start_failed')
      );
      showError(error);
    }
  }, [disabled, pollOnce, stopPolling, t]);

  // 路径 ④：用户取消。
  const cancel = useCallback(() => {
    if (popupRef.current && !popupRef.current.closed) {
      popupRef.current.close();
    }
    reset('idle');
    setAuthUrl('');
  }, [reset]);

  const seconds = Math.floor(elapsed / 1000);
  const progress = Math.min(100, Math.round((elapsed / POLL_TIMEOUT_MS) * 100));

  // 标题取渠道展示名（descriptor 下发，本地化）；说明优先取 descriptor，缺省回退通用文案。
  const title = pickI18n(descriptor && descriptor.name_i18n, descriptor && descriptor.name, lang);
  const panel = (descriptor && descriptor.panel) || {};
  const description =
    pickI18n(panel.description_i18n, panel.description, lang) ||
    t('channel.edit.oauth.description');

  return (
    <Segment color='teal' style={{ marginBottom: '1em' }}>
      <div style={{ fontWeight: 600, marginBottom: '0.4em' }}>
        <Icon name='protect' /> {title}
      </div>
      {description && (
        <div style={{ color: '#666', marginBottom: '0.8em' }}>{description}</div>
      )}

      {phase === 'idle' && (
        <Button
          type='button'
          primary
          disabled={disabled}
          onClick={start}
          icon='sign-in'
          content={t('channel.edit.oauth.button_start')}
        />
      )}
      {phase === 'requesting' && (
        <Button
          type='button'
          loading
          disabled
          content={t('channel.edit.oauth.status_requesting')}
        />
      )}
      {phase === 'waiting' && (
        <>
          <Message info>
            <Message.Header>{t('channel.edit.oauth.status_waiting')}</Message.Header>
            <p>
              {t('channel.edit.oauth.elapsed')}: {seconds}s / {POLL_TIMEOUT_MS / 1000}s
            </p>
            <Progress percent={progress} indicating size='tiny' />
            {retryCount > 0 && (
              <p style={{ color: '#b58105' }}>
                {t('channel.edit.oauth.status_retrying', {
                  count: retryCount,
                  max: POLL_MAX_TRANSIENT_RETRIES,
                })}
              </p>
            )}
            <p style={{ wordBreak: 'break-all' }}>
              {t('channel.edit.oauth.open_manually')}
              <a href={authUrl} target='_blank' rel='noreferrer'>
                {authUrl}
              </a>
            </p>
          </Message>
          <Button
            type='button'
            negative
            onClick={cancel}
            icon='cancel'
            content={t('channel.edit.oauth.button_cancel')}
          />
        </>
      )}
      {phase === 'timeout' && (
        <>
          <Message warning>
            {errorText || t('channel.edit.oauth.status_timeout')}
          </Message>
          <Button
            type='button'
            primary
            disabled={disabled}
            onClick={start}
            icon='redo'
            content={t('channel.edit.oauth.button_retry')}
          />
        </>
      )}
      {phase === 'error' && (
        <>
          <Message error>{errorText || t('channel.edit.oauth.status_error')}</Message>
          <Button
            type='button'
            primary
            disabled={disabled}
            onClick={start}
            icon='redo'
            content={t('channel.edit.oauth.button_retry')}
          />
        </>
      )}
      {phase === 'done' && (
        <Message success>{t('channel.edit.oauth.status_done')}</Message>
      )}

      {phase !== 'done' && (
        <div style={{ color: '#888', marginTop: '0.6em', fontSize: '0.9em' }}>
          {t('channel.edit.oauth.manual_hint')}
        </div>
      )}
    </Segment>
  );
};

// 两个通用渲染器（按 descriptor.panel_type 选择）。当前两条流程在前端同形，故共用同一引擎；
// 保留具名导出以便未来某条流程若需专属 UI（如设备码需展示 user_code）时单独扩展而不影响另一条。
export const OAuthDeviceCodePanel = (props) => <OAuthLoginPanel {...props} />;
export const OAuthAuthorizationCodePanel = (props) => <OAuthLoginPanel {...props} />;

export default OAuthLoginPanel;
