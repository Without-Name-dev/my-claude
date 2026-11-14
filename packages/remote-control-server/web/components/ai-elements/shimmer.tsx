'use client';

import { cn } from '../../src/lib/utils';
import { motion } from 'motion/react';
import { type ElementType, type JSX, memo } from 'react';

export interface TextShimmerProps {
  children: string;
  as?: keyof JSX.IntrinsicElements;
  className?: string;
  duration?: number;
  spread?: number;
}

// Created once per tag: calling motion.create inside the component body made
// a brand-new component type every render, so a streaming "思考中…" pulse
// unmounted and restarted its animation on every chunk.
const motionCache = new Map<string, ElementType>();
function motionFor(tag: string): ElementType {
  let component = motionCache.get(tag);
  if (!component) {
    component = motion.create(tag as keyof JSX.IntrinsicElements);
    motionCache.set(tag, component);
  }
  return component;
}

const ShimmerComponent = ({ children, as = 'p', className, duration = 2 }: TextShimmerProps) => {
  const MotionComponent = motionFor(as);

  return (
    <MotionComponent
      animate={{ opacity: [0.5, 1, 0.5] }}
      className={cn('relative inline-block text-muted-foreground', className)}
      transition={{
        repeat: Number.POSITIVE_INFINITY,
        duration,
        ease: 'easeInOut',
      }}
    >
      {children}
    </MotionComponent>
  );
};

export const Shimmer = memo(ShimmerComponent);
