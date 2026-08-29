import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Form, Label, Modal } from 'semantic-ui-react';
import { API, showError, showSuccess } from '../../helpers';

const ARRAY_FIELDS = [
  'supported_reasoning_levels',
  'input_modalities',
  'output_modalities',
  'supported_endpoint_types',
];

const INT_FIELDS = ['priority', 'context_window', 'max_output_tokens'];

const originInputs = {
  name: '',
  canonical_name: '',
  display_name: '',
  visibility: '',
  supported_in_api: false,
  priority: '',
  default_reasoning_level: '',
  supported_reasoning_levels: '',
  context_window: '',
  truncation_policy: '',
  input_modalities: '',
  output_modalities: '',
  supported_endpoint_types: '',
  max_output_tokens: '',
};

// 逗号分隔文本 -> 字符串数组，兼容中文逗号
const toList = (value) =>
  value
    .split(/[,，]/)
    .map((item) => item.trim())
    .filter((item) => item !== '');

// 空文本按 0 处理，对齐后端 int 零值
const toInt = (value) => {
  const parsed = parseInt(value, 10);
  return Number.isFinite(parsed) ? parsed : 0;
};

const EditMetadata = ({ open, metadata, onClose, onSaved }) => {
  const { t } = useTranslation();
  const isEdit = metadata !== null && metadata !== undefined;
  const [inputs, setInputs] = useState(originInputs);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!open) return;
    if (isEdit && metadata) {
      const next = { ...originInputs };
      Object.keys(originInputs).forEach((key) => {
        const value = metadata[key];
        if (ARRAY_FIELDS.includes(key)) {
          next[key] = Array.isArray(value) ? value.join(',') : '';
        } else if (INT_FIELDS.includes(key)) {
          next[key] = value === undefined || value === null ? '' : String(value);
        } else if (key === 'supported_in_api') {
          next[key] = !!value;
        } else {
          next[key] = value === undefined || value === null ? '' : String(value);
        }
      });
      setInputs(next);
    } else {
      setInputs(originInputs);
    }
  }, [open, metadata, isEdit]);

  const handleInputChange = (e, { name, value }) => {
    setInputs((inputs) => ({ ...inputs, [name]: value }));
  };

  const handleCheckboxChange = (e, { name, checked }) => {
    setInputs((inputs) => ({ ...inputs, [name]: checked }));
  };

  const submit = async () => {
    if (inputs.name.trim() === '') {
      showError(t('metadata.messages.name_required'));
      return;
    }
    // 按类型显式序列化，避免字符串直接传给后端导致 JSON 绑定失败
    const payload = {
      name: inputs.name.trim(),
      canonical_name: inputs.canonical_name.trim(),
      display_name: inputs.display_name,
      visibility: inputs.visibility,
      supported_in_api: inputs.supported_in_api,
      priority: toInt(inputs.priority),
      default_reasoning_level: inputs.default_reasoning_level,
      supported_reasoning_levels: toList(inputs.supported_reasoning_levels),
      context_window: toInt(inputs.context_window),
      truncation_policy: inputs.truncation_policy,
      input_modalities: toList(inputs.input_modalities),
      output_modalities: toList(inputs.output_modalities),
      supported_endpoint_types: toList(inputs.supported_endpoint_types),
      max_output_tokens: toInt(inputs.max_output_tokens),
    };
    setSubmitting(true);
    let res;
    try {
      if (isEdit) {
        // 后端更新为整行覆盖，需回传表单未覆盖的原始字段
        const body = { ...metadata, ...payload };
        res = await API.put('/api/model-metadata/', body);
      } else {
        res = await API.post('/api/model-metadata/', payload);
      }
      const { success, message } = res.data;
      if (success) {
        showSuccess(
          isEdit
            ? t('metadata.messages.update_success')
            : t('metadata.messages.create_success')
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

  const renderArrayInput = (name, label) => (
    <Form.Field>
      <Form.Input
        label={label}
        name={name}
        placeholder={t(`metadata.edit.${name}_placeholder`)}
        onChange={handleInputChange}
        value={inputs[name]}
        autoComplete='new-password'
      />
    </Form.Field>
  );

  const renderIntInput = (name, label) => (
    <Form.Field>
      <Form.Input
        label={label}
        name={name}
        type='number'
        step='1'
        placeholder={t(`metadata.edit.${name}_placeholder`)}
        onChange={handleInputChange}
        value={inputs[name]}
        autoComplete='new-password'
      />
    </Form.Field>
  );

  return (
    <Modal open={open} onClose={onClose} size='small'>
      <Modal.Header>
        {isEdit ? t('metadata.edit.title_edit') : t('metadata.edit.title_create')}
      </Modal.Header>
      <Modal.Content>
        <Form loading={submitting}>
          <Form.Field>
            <Form.Input
              label={t('metadata.edit.name')}
              name='name'
              placeholder={t('metadata.edit.name_placeholder')}
              onChange={handleInputChange}
              value={inputs.name}
              disabled={isEdit}
              required
              autoComplete='new-password'
            />
            {!isEdit && (
              <Label basic pointing>
                {t('metadata.edit.name_immutable_hint')}
              </Label>
            )}
          </Form.Field>
          <Form.Field>
            <Form.Input
              label={t('metadata.edit.canonical_name')}
              name='canonical_name'
              placeholder={t('metadata.edit.canonical_name_placeholder')}
              onChange={handleInputChange}
              value={inputs.canonical_name}
              autoComplete='new-password'
            />
            <Label basic pointing>
              {t('metadata.edit.canonical_name_tip')}
            </Label>
          </Form.Field>
          <Form.Group widths='equal'>
            <Form.Field>
              <Form.Input
                label={t('metadata.edit.display_name')}
                name='display_name'
                placeholder={t('metadata.edit.display_name_placeholder')}
                onChange={handleInputChange}
                value={inputs.display_name}
                autoComplete='new-password'
              />
            </Form.Field>
            <Form.Field>
              <Form.Input
                label={t('metadata.edit.visibility')}
                name='visibility'
                placeholder={t('metadata.edit.visibility_placeholder')}
                onChange={handleInputChange}
                value={inputs.visibility}
                autoComplete='new-password'
              />
            </Form.Field>
          </Form.Group>
          <Form.Field>
            <Form.Checkbox
              toggle
              label={t('metadata.edit.supported_in_api')}
              name='supported_in_api'
              checked={inputs.supported_in_api}
              onChange={handleCheckboxChange}
            />
          </Form.Field>
          <Form.Group widths='equal'>
            {renderIntInput('priority', t('metadata.edit.priority'))}
            {renderIntInput('context_window', t('metadata.edit.context_window'))}
            {renderIntInput('max_output_tokens', t('metadata.edit.max_output_tokens'))}
          </Form.Group>
          <Form.Group widths='equal'>
            <Form.Field>
              <Form.Input
                label={t('metadata.edit.default_reasoning_level')}
                name='default_reasoning_level'
                placeholder={t('metadata.edit.default_reasoning_level_placeholder')}
                onChange={handleInputChange}
                value={inputs.default_reasoning_level}
                autoComplete='new-password'
              />
            </Form.Field>
            <Form.Field>
              <Form.Input
                label={t('metadata.edit.truncation_policy')}
                name='truncation_policy'
                placeholder={t('metadata.edit.truncation_policy_placeholder')}
                onChange={handleInputChange}
                value={inputs.truncation_policy}
                autoComplete='new-password'
              />
            </Form.Field>
          </Form.Group>
          {renderArrayInput(
            'supported_reasoning_levels',
            t('metadata.edit.supported_reasoning_levels')
          )}
          {renderArrayInput('input_modalities', t('metadata.edit.input_modalities'))}
          {renderArrayInput('output_modalities', t('metadata.edit.output_modalities'))}
          {renderArrayInput(
            'supported_endpoint_types',
            t('metadata.edit.supported_endpoint_types')
          )}
        </Form>
      </Modal.Content>
      <Modal.Actions>
        <Button onClick={onClose}>{t('metadata.buttons.cancel')}</Button>
        <Button positive onClick={submit} loading={submitting}>
          {t('metadata.buttons.submit')}
        </Button>
      </Modal.Actions>
    </Modal>
  );
};

export default EditMetadata;
