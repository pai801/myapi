import React, { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Form,
  Header,
  Message,
} from 'semantic-ui-react';
import { Link } from 'react-router-dom';
import {
  API,
  copy,
  showError,
  showSuccess,
} from '../helpers';

const PersonalSetting = () => {
  const { t } = useTranslation();
  const [systemToken, setSystemToken] = useState('');

  const generateAccessToken = async () => {
    const res = await API.get('/api/user/token');
    const { success, message, data } = res.data;
    if (success) {
      setSystemToken(data);
      await copy(data);
      showSuccess(`令牌已重置并已复制到剪贴板`);
    } else {
      showError(message);
    }
  };

  const handleSystemTokenClick = async (e) => {
    e.target.select();
    await copy(e.target.value);
    showSuccess(`系统令牌已复制到剪切板`);
  };

  return (
    <div style={{ lineHeight: '40px' }}>
      <Header as='h3'>{t('setting.personal.general.title')}</Header>
      <Message>{t('setting.personal.general.system_token_notice')}</Message>
      <Button as={Link} to={`/user/edit/`}>
        {t('setting.personal.general.buttons.update_profile')}
      </Button>
      <Button onClick={generateAccessToken}>
        {t('setting.personal.general.buttons.generate_token')}
      </Button>

      {systemToken && (
        <Form.Input
          fluid
          readOnly
          value={systemToken}
          onClick={handleSystemTokenClick}
          style={{ marginTop: '10px' }}
        />
      )}
    </div>
  );
};

export default PersonalSetting;
