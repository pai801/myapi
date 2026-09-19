import React from 'react';
import { Message, Segment } from 'semantic-ui-react';
import { useTranslation } from 'react-i18next';
import {
  descriptorKeyPrompt,
  pickI18n,
  resolvePanelType,
} from '../helpers/channelDescriptor';
import {
  OAuthAuthorizationCodePanel,
  OAuthDeviceCodePanel,
} from './OAuthLoginPanel';

// 通用面板渲染器（PRD §5.12 层2 / 决策 D7）。
//
// 按 descriptor.panel_type 选择渲染器，元数据全部来自后端下发的 ChannelDescriptor，
// 前端不写任何渠道专属分支。已实现：
//   - manual（手写密钥框引导）：渠道名 / 密钥提示 / 默认 BaseURL 均按清单渲染。
//   - oauth-device-code / oauth-authorization-code：通用 OAuth 登录面板（路径与参数由
//     清单的 panel.oauth 下发）。
// 未知（未实现）类型一律回退 manual —— 密钥框必须保持可手写（方案 A 降级路径）。
//
// 说明：实际的手写密钥输入框由 EditChannel 的通用表单字段承载（对所有渠道类型一致），
// oauth 面板成功后通过 onCredential 把凭证回填到该密钥框（不直接写库）。
const ChannelPanelRenderer = ({ descriptor, onCredential, disabled }) => {
  const { t, i18n } = useTranslation();
  if (!descriptor) return null;

  const panelType = resolvePanelType(descriptor);
  if (panelType === 'oauth-device-code') {
    return (
      <OAuthDeviceCodePanel
        descriptor={descriptor}
        onCredential={onCredential}
        disabled={disabled}
      />
    );
  }
  if (panelType === 'oauth-authorization-code') {
    return (
      <OAuthAuthorizationCodePanel
        descriptor={descriptor}
        onCredential={onCredential}
        disabled={disabled}
      />
    );
  }
  return <ManualPanel descriptor={descriptor} t={t} lang={i18n.language} />;
};

function ManualPanel({ descriptor, t, lang }) {
  const name = pickI18n(descriptor.name_i18n, descriptor.name, lang);
  const description = pickI18n(
    descriptor.panel && descriptor.panel.description_i18n,
    descriptor.panel && descriptor.panel.description,
    lang
  );
  const keyPrompt = descriptorKeyPrompt(descriptor, lang);

  return (
    <Segment color='blue' style={{ marginBottom: '1em' }}>
      <div style={{ fontWeight: 600, marginBottom: '0.4em' }}>{name}</div>
      {description && (
        <div style={{ color: '#666', marginBottom: '0.6em' }}>{description}</div>
      )}
      {keyPrompt && <Message info>{keyPrompt}</Message>}
      {descriptor.default_base_url && (
        <div style={{ color: '#888', fontSize: '0.9em' }}>
          {t('channel.edit.descriptor.default_base_url')}:{' '}
          {descriptor.default_base_url}
        </div>
      )}
    </Segment>
  );
}

export default ChannelPanelRenderer;
