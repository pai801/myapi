import React from 'react';
import { Card } from 'semantic-ui-react';
import MetadataTable from './MetadataTable';
import { useTranslation } from 'react-i18next';

const Metadata = () => {
  const { t } = useTranslation();

  return (
    <div className='dashboard-container'>
      <Card fluid className='chart-card'>
        <Card.Content>
          <Card.Header className='header'>{t('metadata.management')}</Card.Header>
          <MetadataTable />
        </Card.Content>
      </Card>
    </div>
  );
};

export default Metadata;
