import React from 'react';
import { Card } from 'semantic-ui-react';
import GroupTable from './GroupTable';
import { useTranslation } from 'react-i18next';

const Group = () => {
  const { t } = useTranslation();

  return (
    <div className='dashboard-container'>
      <Card fluid className='chart-card'>
        <Card.Content>
          <Card.Header className='header'>{t('group.management')}</Card.Header>
          <GroupTable />
        </Card.Content>
      </Card>
    </div>
  );
};

export default Group;
