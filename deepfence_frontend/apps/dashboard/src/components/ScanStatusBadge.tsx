import { capitalize } from 'lodash-es';
import { cn } from 'tailwind-preset';
import { CircleSpinner } from 'ui-components';

import {
  ErrorIcon,
  NotStartedIcon,
  SuccessIcon,
} from '@/components/icons/common/ScanStatuses';
import {
  isNeverScanned,
  isScanComplete,
  isScanFailed,
  isScanInProgress,
} from '@/utils/scan';

export const ScanStatusBadge = ({
  status,
  className,
  justIcon = false,
}: {
  status: string;
  className?: string;
  justIcon?: boolean;
}) => {
  const wrapperClassName = cn(
    'flex items-center gap-1.5 dark:text-text-text-and-icon text-p4',
    className,
  );

  const iconWrapper = cn('w-[18px] h-[18px]');

  const scanStatus = capitalize(status.replaceAll('_', ' '));

  if (isScanComplete(status)) {
    return (
      <div className={wrapperClassName}>
        <span className={iconWrapper}>
          <SuccessIcon />
        </span>

        {!justIcon ? scanStatus : null}
      </div>
    );
  } else if (isScanFailed(status)) {
    return (
      <div className={wrapperClassName}>
        <span className={iconWrapper}>
          <ErrorIcon />
        </span>
        {!justIcon ? scanStatus : null}
      </div>
    );
  } else if (isNeverScanned(status)) {
    return (
      <div className={wrapperClassName}>
        <span className={iconWrapper}>
          <NotStartedIcon />
        </span>
        Never Scanned
      </div>
    );
  } else if (isScanInProgress(status)) {
    return (
      <div className={wrapperClassName}>
        <span className={iconWrapper}>
          <CircleSpinner size="sm" />
        </span>
        {!justIcon ? scanStatus : null}
      </div>
    );
  }
  return null;
};