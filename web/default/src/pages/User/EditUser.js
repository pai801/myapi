import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Form, Card, Label } from 'semantic-ui-react';
import { useParams, useNavigate, useLocation } from 'react-router-dom';
import { API, showError, showSuccess } from '../../helpers';

const EditUser = () => {
  const { t } = useTranslation();
  const params = useParams();
  const userId = params.id;
  const location = useLocation();
  const navigate = useNavigate();
  const userFromList = location.state?.user;
  const [loading, setLoading] = useState(true);
  const [inputs, setInputs] = useState({
    username: userFromList?.username || '',
    display_name: userFromList?.display_name || '',
    password: '',
    quota: userFromList?.quota || 0,
  });
  const {
    username,
    display_name,
    password,
    quota,
  } = inputs;
  const [quotaError, setQuotaError] = useState('');
  const handleInputChange = (e, { name, value }) => {
    if (name === 'quota') {
      setQuotaError('');
    }
    setInputs((inputs) => ({ ...inputs, [name]: value }));
  };
  const handleCancel = () => {
    navigate('/user', { state: { skipRefresh: true } });
  };
  const loadUser = async () => {
    // Always fetch latest data from API; route state is only used as initial placeholder
    let res = undefined;
    if (userId) {
      res = await API.get(`/api/user/${userId}`);
    } else {
      res = await API.get(`/api/user/self`);
    }
    const { success, message, data } = res.data;
    if (success) {
      data.password = '';
      setInputs(data);
    } else {
      showError(message);
    }
    setLoading(false);
  };
  useEffect(() => {
    loadUser().then();
  }, []);

  const submit = async () => {
    const quotaValue = typeof quota === 'string' ? quota.trim() : `${quota}`;
    if (quotaValue === '') {
	    setQuotaError(t('user.edit.quota_invalid'));
	    return;
	  }
    const parsedQuota = Number(quotaValue);
    if (!Number.isFinite(parsedQuota)) {
      setQuotaError(t('user.edit.quota_invalid'));
      return;
    }
    setQuotaError('');
    let res = undefined;
    if (userId) {
      let data = { ...inputs, id: parseInt(userId), quota: parsedQuota };
      res = await API.put(`/api/user/`, data);
    } else {
      res = await API.put(`/api/user/self`, inputs);
    }
    const { success, message } = res.data;
    if (success) {
      showSuccess(t('user.messages.update_success'));
      if (userId) {
        navigate('/user', {
          state: {
            skipRefresh: true,
            updatedUser: {
              ...inputs,
              id: parseInt(userId),
              quota: parsedQuota,
            },
          },
        });
      } else {
        navigate('/setting');
      }
    } else {
      showError(message);
    }
  };

  return (
    <div className='dashboard-container'>
      <Card fluid className='chart-card'>
        <Card.Content>
          <Card.Header className='header'>{t('user.edit.title')}</Card.Header>
          <Form loading={loading} autoComplete='new-password'>
            <Form.Field>
              <Form.Input
                label={t('user.edit.username')}
                name='username'
                placeholder={t('user.edit.username_placeholder')}
                onChange={handleInputChange}
                value={username}
                autoComplete='new-password'
              />
            </Form.Field>
            <Form.Field>
              <Form.Input
                label={t('user.edit.password')}
                name='password'
                type={'password'}
                placeholder={t('user.edit.password_placeholder')}
                onChange={handleInputChange}
                value={password}
                autoComplete='new-password'
              />
            </Form.Field>
            <Form.Field>
              <Form.Input
                label={t('user.edit.display_name')}
                name='display_name'
                placeholder={t('user.edit.display_name_placeholder')}
                onChange={handleInputChange}
                value={display_name}
                autoComplete='new-password'
              />
            </Form.Field>
            <Form.Field error={quotaError}>
              <Form.Input
                label={t('user.edit.quota')}
                name='quota'
                type='number'
                placeholder={t('user.edit.quota_placeholder')}
                onChange={handleInputChange}
                value={quota}
                autoComplete='new-password'
              />
              {quotaError && <Label basic color='red' pointing>{quotaError}</Label>}
            </Form.Field>
            <Button onClick={handleCancel}>
              {t('user.edit.buttons.cancel')}
            </Button>
            <Button positive onClick={submit}>
              {t('user.edit.buttons.submit')}
            </Button>
          </Form>
        </Card.Content>
      </Card>
    </div>
  );
};

export default EditUser;
