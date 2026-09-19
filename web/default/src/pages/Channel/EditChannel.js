import React, {useEffect, useState} from 'react';
import {useTranslation} from 'react-i18next';
import {Button, Card, Form, Input, Message} from 'semantic-ui-react';
import {useNavigate, useParams} from 'react-router-dom';
import {API, copy, getChannelModels, showError, showInfo, showSuccess, showWarning, verifyJSON,} from '../../helpers';
import {renderChannelTip} from '../../helpers/render';
import {
  buildChannelOptions,
  descriptorKeyPrompt,
  findDescriptor,
  getChannelDescriptor,
  loadChannelDescriptors,
} from '../../helpers/channelDescriptor';
import ChannelPanelRenderer from '../../components/ChannelPanelRenderer';

// 渠道类型与元数据全部由后端下发的 ChannelDescriptor 清单驱动（PRD §5.12 / 决策 D7）：
// 前端不再硬编码 54/55/56，也不写渠道专属分支。渠道专属能力（如自定义请求头）
// 由清单的能力位（capabilities.supports_custom_headers）声明，前端据此渲染通用编辑器。

const MODEL_MAPPING_EXAMPLE = {
  'gpt-3.5-turbo-0301': 'gpt-3.5-turbo',
  'gpt-4-0314': 'gpt-4',
  'gpt-4-32k-0314': 'gpt-4-32k',
};

// RFC 7230 tchar 子集：请求头名称允许字母、数字及 !#$%&'*+-.^_`|~（与后端 isValidHeaderName 对齐）。
const HEADER_NAME_RE = /^[A-Za-z0-9!#$%&'*+\-.^_`|~]+$/;

function type2secretPrompt(type, t) {
  switch (type) {
    case 15:
      return t('channel.edit.key_prompts.zhipu');
    case 18:
      return t('channel.edit.key_prompts.spark');
    case 22:
      return t('channel.edit.key_prompts.fastgpt');
    case 23:
      return t('channel.edit.key_prompts.tencent');
    case 53:
      return t('channel.edit.key_prompts.chatgpt_sub');
    default:
      // 扩展渠道等由 ChannelDescriptor 携带 key_prompt，见调用处。
      return t('channel.edit.key_prompts.default');
  }
}

const EditChannel = () => {
  const { t, i18n } = useTranslation();
  const params = useParams();
  const navigate = useNavigate();
  const channelId = params.id;
  const isEdit = channelId !== undefined;
  const [loading, setLoading] = useState(isEdit);
  const handleCancel = () => {
    navigate('/channel');
  };

  const originInputs = {
    name: '',
    type: 1,
    key: '',
    base_url: '',
    other: '',
    model_mapping: '',
    system_prompt: '',
    priority: 1,
    models: [],
    groups: ['default'],
  };
  const [batch, setBatch] = useState(false);
  const [inputs, setInputs] = useState(originInputs);
  const [originModelOptions, setOriginModelOptions] = useState([]);
  const [modelOptions, setModelOptions] = useState([]);
  const [groupOptions, setGroupOptions] = useState([]);
  const [basicModels, setBasicModels] = useState([]);
  const [fullModels, setFullModels] = useState([]);
  const [customModel, setCustomModel] = useState('');
  const [fetchingModels, setFetchingModels] = useState(false);
  const [modelKey, setModelKey] = useState(0);
  const [config, setConfig] = useState({
    region: '',
    sk: '',
    ak: '',
    user_id: '',
    vertex_ai_project_id: '',
    vertex_ai_adc: '',
  });
  // 自定义请求头的行式编辑态：对象无法承载「多个空名行 / 重名行并存」，
  // 故 UI 用有序数组维护，加载时从 config 读取、提交时再折叠回对象写入 config。
  const [customHeaders, setCustomHeaders] = useState([{ name: '', value: '' }]);
  // config 解析失败标记：为 true 时禁止提交，防止用默认 config 静默覆盖库中原始值
  const [configLoadFailed, setConfigLoadFailed] = useState(false);
  // 后端下发的渠道能力清单（PRD §5.12 层1）：驱动渠道类型下拉与通用面板。
  const [descriptors, setDescriptors] = useState([]);
  // 当前所选渠道类型对应的清单（未加载 / 无对应清单时为 undefined）。
  const currentDescriptor = findDescriptor(descriptors, inputs.type);
  // 自定义出站请求头的 config 键名由清单下发（扩展方自选键名），前端不硬编码。
  const customHeadersKey =
    (currentDescriptor &&
      currentDescriptor.capabilities &&
      currentDescriptor.capabilities.custom_headers_key) ||
    '';
  // 渠道专属能力（如自定义出站请求头）由清单能力位声明 + 键名下发，前端据此渲染通用编辑器。
  const supportsCustomHeaders =
    !!(currentDescriptor &&
      currentDescriptor.capabilities &&
      currentDescriptor.capabilities.supports_custom_headers) &&
    customHeadersKey !== '';
  const handleInputChange = (e, { name, value }) => {
    setInputs((inputs) => ({ ...inputs, [name]: value }));
    if (name === 'type') {
      let localModels = getChannelModels(value);
      // inputs.models 可能为 null（旧缓存/后端历史 payload），非数组一律按空处理
      if (!Array.isArray(inputs.models) || inputs.models.length === 0) {
        setInputs((inputs) => ({ ...inputs, models: localModels }));
      }
      setBasicModels(localModels);
    }
  };

  const handleConfigChange = (e, { name, value }) => {
    setConfig((inputs) => ({ ...inputs, [name]: value }));
  };

  // 修改某一行请求头的名称或取值。
  const updateCustomHeader = (index, field, value) => {
    setCustomHeaders((rows) =>
      rows.map((row, i) => (i === index ? { ...row, [field]: value } : row))
    );
  };

  const addCustomHeader = () => {
    setCustomHeaders((rows) => [...rows, { name: '', value: '' }]);
  };

  const removeCustomHeader = (index) => {
    setCustomHeaders((rows) => {
      const next = rows.filter((_, i) => i !== index);
      // 至少保留一行空行，方便直接输入
      return next.length > 0 ? next : [{ name: '', value: '' }];
    });
  };

  // 校验自定义请求头：名称合法且不重复（大小写不敏感）。返回 false 表示已提示并应阻止提交。
  const validateCustomHeaders = () => {
    const seen = new Set();
    for (const row of customHeaders) {
      const name = row.name.trim();
      if (name === '') continue;
      if (!HEADER_NAME_RE.test(name)) {
        showError(t('channel.edit.custom_headers.invalid_name', { name }));
        return false;
      }
      const lower = name.toLowerCase();
      if (seen.has(lower)) {
        showWarning(t('channel.edit.custom_headers.duplicate_name', { name }));
        return false;
      }
      seen.add(lower);
    }
    return true;
  };

  const loadChannel = async () => {
    try {
      let res = await API.get(`/api/channel/${channelId}`);
      const { success, message, data } = res.data;
      if (success) {
        // models 为空串或 null/undefined 都归为 []，否则 null.split(',') 会直接崩溃
        if (data.models === '' || data.models === null || data.models === undefined) {
          data.models = [];
        } else {
          data.models = data.models.split(',');
        }
        if (data.group === '') {
          data.groups = [];
        } else {
          data.groups = data.group.split(',');
        }
        if (data.model_mapping !== '') {
          try {
            data.model_mapping = JSON.stringify(
              JSON.parse(data.model_mapping),
              null,
              2
            );
          } catch (e) {
            // 解析失败时保留原始字符串展示，避免页面空白
            console.warn('channel model_mapping is not valid JSON:', e.message);
          }
        }
        setInputs(data);
        if (data.config !== '') {
          try {
            const parsedConfig = JSON.parse(data.config);
            // 恢复整体替换：仅对「清单声明支持自定义请求头」的渠道补齐其键（清单下发的
            // custom_headers_key）的默认值，其它默认值一律不补，确保不支持该能力的渠道落库形态与改动前逐字节一致。
            const desc = getChannelDescriptor(data.type);
            // 回填键名同样由清单下发，不回退任何硬编码字符串。
            const descHeadersKey =
              (desc &&
                desc.capabilities &&
                desc.capabilities.custom_headers_key) ||
              '';
            if (
              desc &&
              desc.capabilities &&
              desc.capabilities.supports_custom_headers &&
              descHeadersKey !== '' &&
              !parsedConfig[descHeadersKey]
            ) {
              parsedConfig[descHeadersKey] = {};
            }
            setConfig(parsedConfig);
            const headers = descHeadersKey
              ? parsedConfig[descHeadersKey]
              : undefined;
            if (headers && typeof headers === 'object' && !Array.isArray(headers)) {
              const rows = Object.entries(headers).map(([name, value]) => ({
                name,
                value: value === null || value === undefined ? '' : String(value),
              }));
              if (rows.length > 0) {
                setCustomHeaders(rows);
              }
            }
          } catch (e) {
            // 解析失败时保留默认 config 避免页面崩溃，但必须标记失败态，
            // 否则提交时会用默认值静默覆盖库中原始 config
            setConfigLoadFailed(true);
            console.warn('channel config is not valid JSON:', e.message);
            showWarning(
              '渠道 config 解析失败，表单展示的是默认值；为避免覆盖原始配置，本次保存已被阻止'
            );
          }
        }
        setBasicModels(getChannelModels(data.type));
      } else {
        showError(message);
      }
    } finally {
      // 失败时也必须复位 loading，避免表单永久处于加载态
      setLoading(false);
    }
  };

  const fetchModels = async () => {
    try {
      let res = await API.get(`/api/channel/models`);
      let localModelOptions = res.data.data.map((model) => ({
        key: model.id,
        text: model.id,
        value: model.id,
      }));
      setOriginModelOptions(localModelOptions);
      setFullModels(res.data.data.map((model) => model.id));
    } catch (error) {
      showError(error.message);
    }
  };

  const fetchGroups = async () => {
    try {
      let res = await API.get(`/api/group/`);
      setGroupOptions(
        res.data.data.map((group) => ({
          key: group,
          text: group,
          value: group,
        }))
      );
    } catch (error) {
      showError(error.message);
    }
  };

  useEffect(() => {
    let localModelOptions = [...originModelOptions];
    if (!Array.isArray(inputs.models)) {
      setModelOptions(localModelOptions);
      return;
    }
    inputs.models.forEach((model) => {
      if (!localModelOptions.find((option) => option.key === model)) {
        localModelOptions.push({
          key: model,
          text: model,
          value: model,
        });
      }
    });
    setModelOptions(localModelOptions);
  }, [originModelOptions, inputs.models]);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      // 先加载渠道清单：loadChannel 与面板渲染都依赖它。
      // 提交 / 加载渠道走模块缓存 getChannelDescriptor，避免 state 更新的时序问题。
      const list = await loadChannelDescriptors();
      if (cancelled) return;
      setDescriptors(list);
      if (isEdit) {
        // 错误提示已由 api 拦截器统一弹出，此处仅需阻断 unhandled rejection
        await loadChannel();
      } else {
        let localModels = getChannelModels(inputs.type);
        setBasicModels(localModels);
      }
    })().catch(() => {});
    fetchModels().then();
    fetchGroups().then();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const submit = async () => {
    // config 未能成功解析时禁止提交，防止用默认值覆盖库中原始配置
    if (configLoadFailed) {
      showError('渠道 config 解析失败，已阻止保存以避免覆盖原始配置，请刷新页面后重试');
      return;
    }
    if (supportsCustomHeaders && !validateCustomHeaders()) {
      return;
    }
    if (inputs.key === '') {
      if (config.ak !== '' && config.sk !== '' && config.region !== '') {
        inputs.key = `${config.ak}|${config.sk}|${config.region}`;
      } else if (
        config.region !== '' &&
        config.vertex_ai_project_id !== '' &&
        config.vertex_ai_adc !== ''
      ) {
        inputs.key = `${config.region}|${config.vertex_ai_project_id}|${config.vertex_ai_adc}`;
      }
    }
    if (!isEdit && (inputs.name === '' || inputs.key === '')) {
      showInfo(t('channel.edit.messages.name_required'));
      return;
    }
    if (inputs.type !== 43 && (!Array.isArray(inputs.models) || inputs.models.length === 0)) {
      showInfo(t('channel.edit.messages.models_required'));
      return;
    }
    if (inputs.model_mapping !== '' && !verifyJSON(inputs.model_mapping)) {
      showInfo(t('channel.edit.messages.model_mapping_invalid'));
      return;
    }
    let localInputs = { ...inputs };
    if (localInputs.key === 'undefined|undefined|undefined') {
      localInputs.key = ''; // prevent potential bug
    }
    if (localInputs.base_url && localInputs.base_url.endsWith('/')) {
      localInputs.base_url = localInputs.base_url.slice(
        0,
        localInputs.base_url.length - 1
      );
    }
    if (localInputs.type === 3 && localInputs.other === '') {
      localInputs.other = '2024-03-01-preview';
    }
    let res;
    localInputs.models = localInputs.models.join(',');
    localInputs.group = localInputs.groups.join(',');
    // 提交前清洗自定义请求头：剔除空名条目，空对象则删除该字段，避免写入无意义配置。
    const localConfig = { ...config };
    if (supportsCustomHeaders) {
      const headers = {};
      customHeaders.forEach((row) => {
        const name = row.name.trim();
        if (name === '') return;
        // 名称非空即写入；取值为空串也照常写入（headers[name] = ''），
        // 后端据此走 value == "" 分支执行 Header.Del，真实删除该内置头（「留空=不发送」）。
        headers[name] = row.value;
      });
      localConfig[customHeadersKey] = headers;
    }
    if (
      localConfig[customHeadersKey] &&
      typeof localConfig[customHeadersKey] === 'object' &&
      Object.keys(localConfig[customHeadersKey]).length === 0
    ) {
      delete localConfig[customHeadersKey];
    }
    localInputs.config = JSON.stringify(localConfig);
    if (isEdit) {
      res = await API.put(`/api/channel/`, {
        ...localInputs,
        id: parseInt(channelId),
      });
    } else {
      res = await API.post(`/api/channel/`, localInputs);
    }
    const { success, message } = res.data;
    if (success) {
      if (isEdit) {
        showSuccess(t('channel.edit.messages.update_success'));
      } else {
        showSuccess(t('channel.edit.messages.create_success'));
        setInputs(originInputs);
      }
    } else {
      showError(message);
    }
  };

  const addCustomModel = () => {
    if (customModel.trim() === '') return;
    // inputs.models 可能为 null，先归一为数组再使用，避免 .includes / 展开崩溃
    const currentModels = Array.isArray(inputs.models) ? inputs.models : [];
    if (currentModels.includes(customModel)) return;
    let localModels = [...currentModels];
    localModels.push(customModel);
    let localModelOptions = [];
    localModelOptions.push({
      key: customModel,
      text: customModel,
      value: customModel,
    });
    setModelOptions((modelOptions) => {
      return [...modelOptions, ...localModelOptions];
    });
    setCustomModel('');
    handleInputChange(null, { name: 'models', value: localModels });
  };

  const fetchModelsFromBaseURL = async () => {
    // 先记录当前已选中的模型
    const prevSelectedModels = inputs.models;
    setFetchingModels(true);
    try {
      let payload = {
        channel_type: inputs.type,
      };
      if (isEdit) {
        // 编辑渠道：传 channel_id，后端从 DB 取 key 和 base_url
        payload.channel_id = parseInt(channelId);
        payload.base_url = inputs.base_url;
      } else {
        // 新增渠道：前端传 key 和 base_url
        payload.key = inputs.key;
        payload.base_url = inputs.base_url;
      }
      let res = await API.post('/api/channel/fetch_models', payload);
      const { success, message, data } = res.data;
      if (success && Array.isArray(data)) {
        let localModelOptions = data.map((model) => ({
          key: model,
          text: model,
          value: model,
        }));
        // 保留之前已选模型中仍存在于新列表中的部分
        let mergedModels = prevSelectedModels.filter((m) => data.includes(m));
        // 先直接更新 modelOptions，再递增 key 强制下拉框重新挂载
        // 注意：必须在 setModelKey 之前执行，否则 Dropdown 重新挂载时 modelOptions 还是旧值
        setModelOptions(localModelOptions);
        setOriginModelOptions(localModelOptions);
        setFullModels(data);
        setInputs((prev) => ({ ...prev, models: mergedModels }));
        setModelKey((k) => k + 1);
        showSuccess(t('channel.edit.messages.fetch_models_success', { count: data.length }));
      } else {
        showError(message || t('channel.edit.messages.fetch_models_failed'));
      }
    } catch (error) {
      showError(error.message || t('channel.edit.messages.fetch_models_failed'));
    }
    setFetchingModels(false);
  };

  // 渠道类型下拉 = 内置常量 + 后端下发的清单（本仓默认构建不含扩展渠道）。
  const channelOptions = buildChannelOptions(descriptors, i18n.language);
  // 未知 type 兜底（渠道插件化 AC7）：编辑的渠道若为当前构建不认识的类型
  // （已摘除的扩展渠道 / 历史遗留号段），下拉里补一个「未知渠道」项，避免类型选择器
  // 因 value 不在 options 中而渲染成空白 / undefined；管理员仍可改选到其它类型。
  const channelTypeOptions = channelOptions.some(
    (option) => option.value === inputs.type
  )
    ? channelOptions
    : [
        ...channelOptions,
        {
          key: `unknown-type-${inputs.type}`,
          text: t('channel.edit.unknown_type', { type: inputs.type }),
          value: inputs.type,
          color: 'grey',
        },
      ];

  return (
    <div className='dashboard-container'>
      <Card fluid className='chart-card'>
        <Card.Content>
          <Card.Header className='header'>
            {isEdit
              ? t('channel.edit.title_edit')
              : t('channel.edit.title_create')}
          </Card.Header>
          <Form loading={loading} autoComplete='new-password'>
            <Form.Field>
              <Form.Select
                label={t('channel.edit.type')}
                name='type'
                required
                search
                options={channelTypeOptions}
                value={inputs.type}
                onChange={handleInputChange}
              />
            </Form.Field>
            <Form.Field>
              <Form.Input
                label={t('channel.edit.name')}
                name='name'
                placeholder={t('channel.edit.name_placeholder')}
                onChange={handleInputChange}
                value={inputs.name}
                required
              />
            </Form.Field>
            <Form.Field>
              <Form.Dropdown
                label={t('channel.edit.group')}
                placeholder={t('channel.edit.group_placeholder')}
                name='groups'
                required
                fluid
                multiple
                selection
                allowAdditions
                onChange={handleInputChange}
                value={inputs.groups}
                autoComplete='new-password'
                options={groupOptions}
              />
            </Form.Field>
            {renderChannelTip(inputs.type)}

            {/* 渠道专属面板（PRD §5.12 层2 / 决策 D7）：按 ChannelDescriptor 的 panel_type
                渲染通用组件，不写渠道专属分支。oauth-* 面板成功后把凭证回填密钥框
                （onCredential），密钥框始终可手写（降级路径）。 */}
            <ChannelPanelRenderer
              descriptor={currentDescriptor}
              onCredential={(credential) =>
                setInputs((prev) => ({ ...prev, key: credential }))
              }
              disabled={loading}
            />

            {/* 自定义出站请求头：由清单能力位 supports_custom_headers 声明，通用编辑器。 */}
            {supportsCustomHeaders && (
              <Form.Field>
                <label>{t('channel.edit.custom_headers.title')}</label>
                <div
                  style={{
                    color: '#666',
                    fontWeight: 'normal',
                    marginBottom: '0.6em',
                  }}
                >
                  {t('channel.edit.custom_headers.description')}
                </div>
                {customHeaders.map((row, index) => (
                  <div
                    key={index}
                    style={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: '0.5em',
                      marginBottom: '0.5em',
                    }}
                  >
                    <Input
                      style={{ flex: 1 }}
                      placeholder={t(
                        'channel.edit.custom_headers.name_placeholder'
                      )}
                      value={row.name}
                      onChange={(e, { value }) =>
                        updateCustomHeader(index, 'name', value)
                      }
                      autoComplete='new-password'
                    />
                    <Input
                      style={{ flex: 1 }}
                      placeholder={t(
                        'channel.edit.custom_headers.value_placeholder'
                      )}
                      value={row.value}
                      onChange={(e, { value }) =>
                        updateCustomHeader(index, 'value', value)
                      }
                      autoComplete='new-password'
                    />
                    <Button
                      type='button'
                      icon='trash'
                      aria-label={t('channel.edit.custom_headers.delete')}
                      onClick={() => removeCustomHeader(index)}
                    />
                  </div>
                ))}
                <Button
                  type='button'
                  icon='plus'
                  content={t('channel.edit.custom_headers.add')}
                  onClick={addCustomHeader}
                />
              </Form.Field>
            )}

            {/* Azure OpenAI specific fields */}
            {inputs.type === 3 && (
              <>
                <Message>
                  注意，<strong>模型部署名称必须和模型名称保持一致</strong>
                  ，因为 MyApi 会把请求体中的 model
                  参数替换为你的部署名称（模型名称中的点会被剔除），
                  <a
                    target='_blank'
                    href='https://github.com/songquanpeng/one-api/issues/133?notification_referrer_id=NT_kwDOAmJSYrM2NjIwMzI3NDgyOjM5OTk4MDUw#issuecomment-1571602271'
                  >
                    图片演示
                  </a>
                  。
                </Message>
                <Form.Field>
                  <Form.Input
                    label='AZURE_OPENAI_ENDPOINT'
                    name='base_url'
                    placeholder='请输入 AZURE_OPENAI_ENDPOINT，例如：https://docs-test-001.openai.azure.com'
                    onChange={handleInputChange}
                    value={inputs.base_url}
                    autoComplete='new-password'
                  />
                </Form.Field>
                <Form.Field>
                  <Form.Input
                    label='默认 API 版本'
                    name='other'
                    placeholder='请输入默认 API 版本，例如：2024-03-01-preview，该配置可以被实际的请求查询参数所覆盖'
                    onChange={handleInputChange}
                    value={inputs.other}
                    autoComplete='new-password'
                  />
                </Form.Field>
              </>
            )}

            {/* Custom base URL field */}
            {inputs.type === 8 && (
              <Form.Field>
                <Form.Input
                    required
                    label={t('channel.edit.proxy_url')}
                    name='base_url'
                    placeholder={t('channel.edit.proxy_url_placeholder')}
                    onChange={handleInputChange}
                    value={inputs.base_url}
                    autoComplete='new-password'
                />
              </Form.Field>
            )}
            {inputs.type === 50 && (
                <Form.Field>
                  <Form.Input
                      required
                  label={t('channel.edit.base_url')}
                  name='base_url'
                  placeholder={t('channel.edit.base_url_placeholder')}
                  onChange={handleInputChange}
                  value={inputs.base_url}
                  autoComplete='new-password'
                />
              </Form.Field>
            )}
            {inputs.type === 52 && (
                <Form.Field>
                  <Form.Input
                      required
                  label={t('channel.edit.base_url')}
                  name='base_url'
                  placeholder={t('channel.edit.base_url_placeholder')}
                  onChange={handleInputChange}
                  value={inputs.base_url}
                  autoComplete='new-password'
                />
              </Form.Field>
            )}

            {inputs.type === 18 && (
              <Form.Field>
                <Form.Input
                  label={t('channel.edit.spark_version')}
                  name='other'
                  placeholder={t('channel.edit.spark_version_placeholder')}
                  onChange={handleInputChange}
                  value={inputs.other}
                  autoComplete='new-password'
                />
              </Form.Field>
            )}
            {inputs.type === 21 && (
              <Form.Field>
                <Form.Input
                  label={t('channel.edit.knowledge_id')}
                  name='other'
                  placeholder={t('channel.edit.knowledge_id_placeholder')}
                  onChange={handleInputChange}
                  value={inputs.other}
                  autoComplete='new-password'
                />
              </Form.Field>
            )}
            {inputs.type === 17 && (
              <Form.Field>
                <Form.Input
                  label={t('channel.edit.plugin_param')}
                  name='other'
                  placeholder={t('channel.edit.plugin_param_placeholder')}
                  onChange={handleInputChange}
                  value={inputs.other}
                  autoComplete='new-password'
                />
              </Form.Field>
            )}
            {inputs.type === 34 && (
              <Message>{t('channel.edit.coze_notice')}</Message>
            )}
            {inputs.type === 40 && (
              <Message>
                {t('channel.edit.douban_notice')}
                <a
                  target='_blank'
                  href='https://console.volcengine.com/ark/region:ark+cn-beijing/endpoint'
                >
                  {t('channel.edit.douban_notice_link')}
                </a>
                {t('channel.edit.douban_notice_2')}
              </Message>
            )}
            {inputs.type !== 43 && (
              <Form.Field>
                <Form.Dropdown
                  key={modelKey}
                  label={t('channel.edit.models')}
                  placeholder={t('channel.edit.models_placeholder')}
                  name='models'
                  required
                  fluid
                  multiple
                  search
                  onLabelClick={(e, { value }) => {
                    copy(value).then();
                  }}
                  selection
                  onChange={handleInputChange}
                  value={inputs.models}
                  autoComplete='new-password'
                  options={modelOptions}
                />
              </Form.Field>
            )}
            {inputs.type !== 43 && (
              <div style={{ lineHeight: '40px', marginBottom: '12px' }}>
                <Button
                  type={'button'}
                  loading={fetchingModels}
                  onClick={fetchModelsFromBaseURL}
                >
                  {t('channel.edit.buttons.fetch_models')}
                </Button>
                <Button
                  type={'button'}
                  onClick={() => {
                    handleInputChange(null, {
                      name: 'models',
                      value: basicModels,
                    });
                  }}
                >
                  {t('channel.edit.buttons.fill_models')}
                </Button>
                <Button
                  type={'button'}
                  onClick={() => {
                    handleInputChange(null, {
                      name: 'models',
                      value: fullModels,
                    });
                  }}
                >
                  {t('channel.edit.buttons.fill_all')}
                </Button>
                <Button
                  type={'button'}
                  onClick={() => {
                    handleInputChange(null, { name: 'models', value: [] });
                  }}
                >
                  {t('channel.edit.buttons.clear')}
                </Button>
                <Input
                  action={
                    <Button type={'button'} onClick={addCustomModel}>
                      {t('channel.edit.buttons.add_custom')}
                    </Button>
                  }
                  placeholder={t('channel.edit.buttons.custom_placeholder')}
                  value={customModel}
                  onChange={(e, { value }) => {
                    setCustomModel(value);
                  }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      addCustomModel();
                      e.preventDefault();
                    }
                  }}
                />
              </div>
            )}
            {inputs.type !== 43 && (
              <>
                <Form.Field>
                  <Form.TextArea
                    label={t('channel.edit.model_mapping')}
                    placeholder={`${t(
                      'channel.edit.model_mapping_placeholder'
                    )}\n${JSON.stringify(MODEL_MAPPING_EXAMPLE, null, 2)}`}
                    name='model_mapping'
                    onChange={handleInputChange}
                    value={inputs.model_mapping}
                    style={{
                      minHeight: 150,
                      fontFamily: 'JetBrains Mono, Consolas',
                    }}
                    autoComplete='new-password'
                  />
                </Form.Field>
                <Form.Field>
                  <Form.TextArea
                    label={t('channel.edit.system_prompt')}
                    placeholder={t('channel.edit.system_prompt_placeholder')}
                    name='system_prompt'
                    onChange={handleInputChange}
                    value={inputs.system_prompt}
                    style={{
                      minHeight: 150,
                      fontFamily: 'JetBrains Mono, Consolas',
                    }}
                    autoComplete='new-password'
                  />
                </Form.Field>
              </>
            )}
            {inputs.type === 33 && (
              <Form.Field>
                <Form.Input
                  label='Region'
                  name='region'
                  required
                  placeholder={t('channel.edit.aws_region_placeholder')}
                  onChange={handleConfigChange}
                  value={config.region}
                  autoComplete=''
                />
                <Form.Input
                  label='AK'
                  name='ak'
                  required
                  placeholder={t('channel.edit.aws_ak_placeholder')}
                  onChange={handleConfigChange}
                  value={config.ak}
                  autoComplete=''
                />
                <Form.Input
                  label='SK'
                  name='sk'
                  required
                  placeholder={t('channel.edit.aws_sk_placeholder')}
                  onChange={handleConfigChange}
                  value={config.sk}
                  autoComplete=''
                />
              </Form.Field>
            )}
            {inputs.type === 42 && (
              <Form.Field>
                <Form.Input
                  label='Region'
                  name='region'
                  required
                  placeholder={t('channel.edit.vertex_region_placeholder')}
                  onChange={handleConfigChange}
                  value={config.region}
                  autoComplete=''
                />
                <Form.Input
                  label={t('channel.edit.vertex_project_id')}
                  name='vertex_ai_project_id'
                  required
                  placeholder={t('channel.edit.vertex_project_id_placeholder')}
                  onChange={handleConfigChange}
                  value={config.vertex_ai_project_id}
                  autoComplete=''
                />
                <Form.Input
                  label={t('channel.edit.vertex_credentials')}
                  name='vertex_ai_adc'
                  required
                  placeholder={t('channel.edit.vertex_credentials_placeholder')}
                  onChange={handleConfigChange}
                  value={config.vertex_ai_adc}
                  autoComplete=''
                />
              </Form.Field>
            )}
            {inputs.type === 34 && (
              <Form.Input
                label={t('channel.edit.user_id')}
                name='user_id'
                required
                placeholder={t('channel.edit.user_id_placeholder')}
                onChange={handleConfigChange}
                value={config.user_id}
                autoComplete=''
              />
            )}
            {inputs.type !== 33 &&
              inputs.type !== 42 &&
              (batch ? (
                <Form.Field>
                  <Form.TextArea
                    label={t('channel.edit.key')}
                    name='key'
                    required
                    placeholder={t('channel.edit.batch_placeholder')}
                    onChange={handleInputChange}
                    value={inputs.key}
                    style={{
                      minHeight: 150,
                      fontFamily: 'JetBrains Mono, Consolas',
                    }}
                    autoComplete='new-password'
                  />
                </Form.Field>
              ) : (
                <Form.Field>
                  <Form.Input
                    label={t('channel.edit.key')}
                    name='key'
                    required
                    placeholder={
                      descriptorKeyPrompt(currentDescriptor, i18n.language) ||
                      type2secretPrompt(inputs.type, t)
                    }
                    onChange={handleInputChange}
                    value={inputs.key}
                    autoComplete='new-password'
                  />
                </Form.Field>
              ))}
            {inputs.type === 37 && (
              <Form.Field>
                <Form.Input
                  label='Account ID'
                  name='user_id'
                  required
                  placeholder={
                    '请输入 Account ID，例如：d8d7c61dbc334c32d3ced580e4bf42b4'
                  }
                  onChange={handleConfigChange}
                  value={config.user_id}
                  autoComplete=''
                />
              </Form.Field>
            )}
            {inputs.type !== 33 && !isEdit && (
              <Form.Checkbox
                checked={batch}
                label={t('channel.edit.batch')}
                name='batch'
                onChange={() => setBatch(!batch)}
              />
            )}
            {inputs.type !== 3 &&
              inputs.type !== 33 &&
              inputs.type !== 8 &&
                inputs.type !== 50 &&
              inputs.type !== 52 &&
              inputs.type !== 22 && (
                <Form.Field>
                  <Form.Input
                      label={t('channel.edit.proxy_url')}
                    name='base_url'
                      placeholder={t('channel.edit.proxy_url_placeholder')}
                    onChange={handleInputChange}
                    value={inputs.base_url}
                    autoComplete='new-password'
                  />
                </Form.Field>
              )}
            {inputs.type === 22 && (
              <Form.Field>
                <Form.Input
                  label='私有部署地址'
                  name='base_url'
                  placeholder={
                    '请输入私有部署地址，格式为：https://fastgpt.run/api/openapi'
                  }
                  onChange={handleInputChange}
                  value={inputs.base_url}
                  autoComplete='new-password'
                />
              </Form.Field>
            )}
            <Button onClick={handleCancel}>
              {t('channel.edit.buttons.cancel')}
            </Button>
            <Button
              type={isEdit ? 'button' : 'submit'}
              positive
              onClick={submit}
            >
              {t('channel.edit.buttons.submit')}
            </Button>
          </Form>
        </Card.Content>
      </Card>
    </div>
  );
};

export default EditChannel;
