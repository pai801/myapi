import React, { useContext, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Form,
  Header,
  Message,
  Modal,
} from 'semantic-ui-react';
import { Link, useNavigate } from 'react-router-dom';
import {
  API,
  copy,
  showError,
  showSuccess,
} from '../helpers';
import Turnstile from 'react-turnstile';
import { UserContext } from '../context/User';

const PersonalSetting = () => {
  const { t } = useTranslation();
  const [userState, userDispatch] = useContext(UserContext);
  let navigate = useNavigate();

  const [inputs, setInputs] = useState({
    self_account_deletion_confirmation: '',
  });
  const [status, setStatus] = useState({});
  const [showAccountDeleteModal, setShowAccountDeleteModal] = useState(false);
  const [turnstileEnabled, setTurnstileEnabled] = useState(false);
  const [turnstileSiteKey, setTurnstileSiteKey] = useState('');
  const [turnstileToken, setTurnstileToken] = useState('');
  const [loading, setLoading] = useState(false);
  const [systemToken, setSystemToken] = useState('');

  useEffect(() => {
    let status = localStorage.getItem('status');
    if (status) {
      status = JSON.parse(status);
      setStatus(status);
      if (status.turnstile_check) {
        setTurnstileEnabled(true);
        setTurnstileSiteKey(status.turnstile_site_key);
      }
    }
  }, []);

  const handleInputChange = (e, { name, value }) => {
    setInputs((inputs) => ({ ...inputs, [name]: value }));
  };

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

  const deleteAccount = async () => {
    if (inputs.self_account_deletion_confirmation !== userState.user.username) {
      showError('请输入你的账户名以确认删除！');
      return;
    }

    const res = await API.delete('/api/user/self');
    const { success, message } = res.data;

    if (success) {
      showSuccess('账户已删除！');
      await API.get('/api/user/logout');
      userDispatch({ type: 'logout' });
      localStorage.removeItem('user');
      navigate('/login');
    } else {
      showError(message);
    }
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
      <Button
        onClick={() => {
          setShowAccountDeleteModal(true);
        }}
      >
        {t('setting.personal.general.buttons.delete_account')}
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
      <Modal
        onClose={() => setShowAccountDeleteModal(false)}
        onOpen={() => setShowAccountDeleteModal(true)}
        open={showAccountDeleteModal}
        size={'tiny'}
        style={{ maxWidth: '450px' }}
      >
        <Modal.Header>
          {t('setting.personal.delete_account.title')}
        </Modal.Header>
        <Modal.Content>
          <Message>{t('setting.personal.delete_account.warning')}</Message>
          <Modal.Description>
            <Form size='large'>
              <Form.Input
                fluid
                placeholder={t(
                  'setting.personal.delete_account.confirm_placeholder',
                  {
                    username: userState?.user?.username,
                  }
                )}
                name='self_account_deletion_confirmation'
                value={inputs.self_account_deletion_confirmation}
                onChange={handleInputChange}
              />
              {turnstileEnabled && (
                <Turnstile
                  sitekey={turnstileSiteKey}
                  onVerify={(token) => {
                    setTurnstileToken(token);
                  }}
                />
              )}
              <div
                style={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  marginTop: '1rem',
                }}
              >
                <Button
                  color='red'
                  fluid
                  size='large'
                  onClick={deleteAccount}
                  loading={loading}
                >
                  {t('setting.personal.delete_account.buttons.confirm')}
                </Button>
                <div style={{ width: '1rem' }}></div>
                <Button
                  fluid
                  size='large'
                  onClick={() => setShowAccountDeleteModal(false)}
                >
                  {t('setting.personal.delete_account.buttons.cancel')}
                </Button>
              </div>
            </Form>
          </Modal.Description>
        </Modal.Content>
      </Modal>
    </div>
  );
};

export default PersonalSetting;
