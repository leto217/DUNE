import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Popover, Tag } from 'antd';

import { SizeFormatter } from '@/utils';
import type { ClientRecord } from '@/hooks/useClients';

export interface ClientFupCellProps {
  fup?: ClientRecord['fup'];
  compact?: boolean;
}

function limitGB(bytes: number): number {
  if (!bytes || bytes <= 0) return 0;
  return Math.round((bytes / (1024 * 1024 * 1024)) * 100) / 100;
}

export default function ClientFupCell({ fup, compact = false }: ClientFupCellProps) {
  const { t } = useTranslation();

  const summary = useMemo(() => {
    if (!fup) return null;
    const parts: string[] = [];
    if (fup.dailyLimit && fup.dailyLimit > 0) {
      parts.push(`${t('pages.clients.fupDailyShort')}: ${SizeFormatter.sizeFormat(fup.dailyUsed || 0)} / ${limitGB(fup.dailyLimit)} GB`);
    }
    if (fup.weeklyLimit && fup.weeklyLimit > 0) {
      parts.push(`${t('pages.clients.fupWeeklyShort')}: ${SizeFormatter.sizeFormat(fup.weeklyUsed || 0)} / ${limitGB(fup.weeklyLimit)} GB`);
    }
    if (fup.monthlyLimit && fup.monthlyLimit > 0) {
      parts.push(`${t('pages.clients.fupMonthlyShort')}: ${SizeFormatter.sizeFormat(fup.monthlyUsed || 0)} / ${limitGB(fup.monthlyLimit)} GB`);
    }
    if (parts.length === 0) return null;
    return parts;
  }, [fup, t]);

  if (!summary) {
    return <span style={{ color: 'rgba(0,0,0,0.45)' }}>—</span>;
  }

  const status = fup?.status || 'normal';
  const statusColor = status === 'disabled' ? 'red' : status === 'exceeded' ? 'orange' : 'green';
  const statusLabel = t(`pages.clients.fupStatus.${status}`, { defaultValue: status });

  const primary = summary[0];
  const body = (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4, maxWidth: 280 }}>
      {summary.map((line) => (
        <span key={line} style={{ fontSize: 12 }}>{line}</span>
      ))}
      <Tag color={statusColor} style={{ width: 'fit-content', marginTop: 4 }}>
        {statusLabel}
      </Tag>
    </div>
  );

  if (compact) {
    return (
      <Popover content={body} trigger={['hover', 'click']} placement="top">
        <Tag color={statusColor} style={{ margin: 0, cursor: 'pointer' }}>{statusLabel}</Tag>
      </Popover>
    );
  }

  return (
    <Popover content={body} trigger={['hover', 'click']} placement="top">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 2, cursor: 'pointer' }}>
        <span style={{ fontSize: 12 }}>{primary}</span>
        <Tag color={statusColor} style={{ width: 'fit-content', margin: 0 }}>{statusLabel}</Tag>
      </div>
    </Popover>
  );
}
