import { describe, it, expect } from 'vitest';
import { Doctor } from './Doctor.js';

describe('Doctor', () => {
    it('should check a binary and return status', () => {
        const status = Doctor.checkBinary('node');
        expect(status.found).toBe(true);
        expect(status.name).toBe('node');
    });

    it('should handle missing binaries', () => {
        const status = Doctor.checkBinary('non-existent-binary-123');
        expect(status.found).toBe(false);
        expect(status.instruction).toBeDefined();
    });
});
