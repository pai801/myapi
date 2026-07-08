import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Form, Modal } from 'semantic-ui-react';
import { API, showError, showSuccess } from '../../helpers';

const EditGroup = ({ open, group, onClose, onSaved }) => {
  const { t } = useTranslation();
  const isEdit = group !== null && group !== undefined;
  const originInputs = {
    name: '',
    model_ratio: '1.0',
  };
  const [inputs, setInputs] = useState(originInputs);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!open) return;
    if (isEdit && group) {
      let ratio = group.model_ratio;
      if (ratio === undefined || ratio === null || isNaN(parseFloat(ratio))) {
        ratio = '1.0';
      } else {
        ratio = String(ratio);
      }
      setInputs({ name: group.name || '', model_ratio: ratio });
    } else {
      setInputs(originInputs);
    }
  }, [open, group, isEdit]);

  const handleInputChange = (e, { name, value }) => {
    setInputs((inputs) => ({ ...inputs, [name]: value }));
  };

  const submit = async () => {
    if (inputs.name.trim() === '') {
      showError(t('group.name') + t('channel.edit.messages.name_required'));
      return;
    }
    const ratio = parseFloat(inputs.model_ratio);
    if (isNaN(ratio) || ratio <= 0) {
      showError('倍率必须大于 0');
      return;
    }
    setSubmitting(true);
    let res;
    const body = { name: inputs.name.trim(), model_ratio: ratio };
    try {
      if (isEdit) {
        res = await API.put(`/api/group/${group.id}`, body);
      } else {
        res = await API.post(`/api/group`, body);
      }
      const { success, message } = res.data;
      if (success) {
        showSuccess(
          isEdit ? t('group.edit') + ' ' + t('token.edit.messages.update_success') : t('group.add') + ' ' + t('token.edit.messages.create_success')
        );
        onSaved();
      } else {
        showError(message);
      }
    } catch (error) {
      showError(error.message);
    }
    setSubmitting(false);
  };

  return (
    <Modal open={open} onClose={onClose} size='small'>
      <Modal.Header>
        {isEdit ? t('group.edit') : t('group.add')}
      </Modal.Header>
      <Modal.Content>
        <Form loading={submitting}>
          <Form.Field>
            <Form.Input
              label={t('group.name')}
              name='name'
              placeholder={t('group.name')}
              onChange={handleInputChange}
              value={inputs.name}
              required
              autoComplete='new-password'
            />
          </Form.Field>
          <Form.Field>
            <Form.Input
              label={t('group.model_ratio')}
              placeholder='1.5'
              name='model_ratio'
              type='number'
              step='0.01'
              min='0.01'
              onChange={handleInputChange}
              value={inputs.model_ratio}
              autoComplete='new-password'
            />
          </Form.Field>
        </Form>
      </Modal.Content>
      <Modal.Actions>
        <Button onClick={onClose}>{t('token.edit.buttons.cancel')}</Button>
        <Button positive onClick={submit} loading={submitting}>
          {t('token.edit.buttons.submit')}
        </Button>
      </Modal.Actions>
    </Modal>
  );
};

export default EditGroup;
