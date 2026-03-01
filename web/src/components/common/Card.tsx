import React from 'react';

export interface CardProps {
  children: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
}

export const Card: React.FC<CardProps> = ({ children, className = '', style }) => {
  return <div className={`card ${className}`} style={style}>{children}</div>;
};

interface CardHeaderProps {
  title?: React.ReactNode;
  action?: React.ReactNode;
  children?: React.ReactNode;
}

export const CardHeader: React.FC<CardHeaderProps> = ({ title, action, children }) => {
  return (
    <div className="card-header">
      <h3 className="card-title">{title || children}</h3>
      {action && <div>{action}</div>}
    </div>
  );
};

export interface CardBodyProps {
  children: React.ReactNode;
  className?: string;
  style?: React.CSSProperties;
}

export const CardBody: React.FC<CardBodyProps> = ({ children, className = '', style }) => {
  return <div className={`card-body ${className}`} style={style}>{children}</div>;
};

interface CardFooterProps {
  children: React.ReactNode;
}

export const CardFooter: React.FC<CardFooterProps> = ({ children }) => {
  return <div className="card-footer">{children}</div>;
};

export default Card;
